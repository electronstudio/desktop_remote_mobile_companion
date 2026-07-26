package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alexflint/go-arg"
	"github.com/electronstudio/desktop_remote_mobile_companion/input"
	"github.com/electronstudio/desktop_remote_mobile_companion/tablet"
	"github.com/electronstudio/desktop_remote_mobile_companion/trackpad"
	"github.com/electronstudio/desktop_remote_mobile_companion/video"
	"github.com/electronstudio/low_latency_dictation/toast"
	"github.com/gorilla/websocket"
	"github.com/mdp/qrterminal/v3"
	"github.com/pion/webrtc/v4"
)

//go:embed static/*
var staticFS embed.FS

//go:embed VERSION
var version string

// videoEnabled is the startup decision about whether to attempt desktop
// video streaming for this run. It is computed in main from --no-video and
// the CAP_SYS_ADMIN check, and read by signalHandler.
var videoEnabled bool

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var cli struct {
	Port int `arg:"-p,--port" default:"8080" help:"HTTPS listen port"`

	NoVideo    bool   `arg:"--no-video" default:"false" help:"disable desktop video streaming"`
	VideoCard  string `arg:"--video-card" default:"" help:"DRM card to capture (e.g. /dev/dri/card1); empty auto-detects"`
	VideoFps   int    `arg:"--video-fps" default:"30" help:"video capture frame rate"`
	VideoQP    int    `arg:"--video-qp" default:"24" help:"h264_vaapi constant-quality QP"`
	VideoWidth int    `arg:"--video-width" default:"0" help:"cap video output width; 0 native (reserved for future)"`
	LowPower   int    `arg:"--low-power" default:"0" help:"h264_vaapi low-power mode (0 or 1)"`

	NoTabletKeepalive bool `arg:"--no-tablet-keepalive" default:"false" help:"disable the tablet hover keep-alive (Mutter cooldown workaround); use on compositors without the cooldown (e.g. wlroots) so the mouse is not grabbed while idle"`
}

type signalMsg struct {
	Type             string  `json:"type"` // offer | answer | candidate
	SDP              string  `json:"sdp,omitempty"`
	Candidate        string  `json:"candidate,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

func main() {
	arg.MustParse(&cli)
	listenAddr := fmt.Sprintf(":%d", cli.Port)
	versionStr := strings.TrimSpace(version)

	if err := toast.Init(nil); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}

	// videoEnabled is the effective decision about whether desktop video
	// streaming will be attempted. It starts as the user's --no-video choice
	// and may be cleared at startup if the process lacks CAP_SYS_ADMIN, which
	// kmsgrab needs to acquire DRM master and map the framebuffer. Keeping it
	// as a package var lets signalHandler avoid re-running the check.
	videoEnabled = !cli.NoVideo

	log.Printf("Desktop Remote Mobile Companion v%s", versionStr)

	certDir, err := certDirectory()
	if err != nil {
		log.Fatalf("certificate directory failed: %v", err)
	}
	certPath := filepath.Join(certDir, "server.crt")
	keyPath := filepath.Join(certDir, "server.key")

	cert, fingerprint, err := loadOrGenerateCert(certPath, keyPath)
	if err != nil {
		log.Fatalf("certificate setup failed: %v", err)
	}

	pad, err := trackpad.New()
	if err != nil {
		toast.Show("failed to register virtual trackpad", uinputInstructions, true)
		log.Fatalf("failed to register virtual trackpad: %v\n\n%s", err, uinputInstructions)
	}
	defer pad.Close()

	tabletDev, err := tablet.New(!cli.NoTabletKeepalive)
	if err != nil {
		toast.Show("failed to register virtual graphics tablet", uinputInstructions, true)
		log.Fatalf("failed to register virtual graphics tablet: %v\n\n%s", err, uinputInstructions)
	}
	defer tabletDev.Close()

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("static filesystem failed: %v", err)
	}

	indexBytes, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		log.Fatalf("failed to read embedded index.html: %v", err)
	}
	indexHTML := strings.ReplaceAll(string(indexBytes), "{{VERSION}}", versionStr)
	fileServer := http.FileServer(http.FS(staticSub))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Write([]byte(indexHTML))
			return
		}
		fileServer.ServeHTTP(w, r)
	})
	http.HandleFunc("/signal", func(w http.ResponseWriter, r *http.Request) {
		signalHandler(w, r, pad, tabletDev)
	})

	if !cli.NoVideo {
		// kmsgrab requires CAP_SYS_ADMIN. Without it every phone connection
		// would fail with a cryptic "No handle set on framebuffer" / EINVAL
		// error from FFmpeg. Detect that once up front, tell the user how to
		// fix it, and silently disable video for this run so trackpad/tablet
		// keep working without per-connection noise.
		if ok, err := hasCapSysAdmin(); err != nil {
			log.Printf("warning: could not check CAP_SYS_ADMIN (%v); desktop video may fail", err)
		} else if !ok {
			toast.Show("warning: desktop video streaming disabled", videoMissingCapInstructions, false)
			log.Printf("warning: desktop video streaming disabled: the process lacks CAP_SYS_ADMIN, which kmsgrab needs to capture the framebuffer.")
			// File capabilities (setcap) are the preferred fix, but they are
			// silently ignored on nosuid mounts, so give the right advice for
			// the actual situation instead of sending the user down a blind
			// alley. sudo works regardless (root has the cap in its bounding set).
			if nosuid, ferr := onNoSuidMount(); ferr != nil {
				log.Printf("note: could not determine whether the executable is on a nosuid mount (%v)", ferr)
				log.Print(videoMissingCapInstructions)
			} else if nosuid {
				log.Print(videoMissingCapOnNoSuidMountInstructions)
			} else {
				log.Print(videoMissingCapInstructions)
			}
			videoEnabled = false
		}
	}

	if videoEnabled {
		log.Printf("desktop video streaming enabled (VAAPI/kmsgrab)")
		if cli.VideoCard != "" {
			log.Printf("  capture card: %s", cli.VideoCard)
		} else {
			log.Printf("  capture card: auto-detect")
		}
		log.Printf("  fps=%d qp=%d low-power=%d", cli.VideoFps, cli.VideoQP, cli.LowPower)
	} else if cli.NoVideo {
		log.Printf("desktop video streaming disabled (--no-video)")
	} else {
		log.Printf("desktop video streaming disabled (missing CAP_SYS_ADMIN)")
	}

	log.Printf("HTTPS listening on https://localhost%s", listenAddr)
	log.Printf(" certificate stored in %s", certDir)
	log.Printf(" certificate SHA-256 fingerprint: %s", fingerprint)

	for _, ip := range localIPs(true) {
		s := fmt.Sprintf("https://%s:%d", ip, cli.Port)
		log.Printf(" also reachable at %s", s)

		qrterminal.GenerateHalfBlock(s, qrterminal.L, os.Stdout)
	}

	server := &http.Server{
		Addr: listenAddr,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}
	log.Fatal(server.ListenAndServeTLS("", ""))
}

func signalHandler(w http.ResponseWriter, r *http.Request, pad trackpad.Device, tabletDev tablet.Device) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}
	defer ws.Close()

	log.Printf("signal connection from %s", r.RemoteAddr)

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Printf("peer connection creation failed: %v", err)
		return
	}
	defer pc.Close()

	var writeMu sync.Mutex
	write := func(msg signalMsg) {
		b, _ := json.Marshal(msg)
		writeMu.Lock()
		ws.WriteMessage(websocket.TextMessage, b)
		writeMu.Unlock()
	}

	// videoStreamer holds the desktop capture pipeline for this connection.
	// It is created only when the peer's offer contains a video media section
	// and video is not disabled. It is nil otherwise.
	var videoStreamer video.Streamer

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("data channel received from %s", r.RemoteAddr)
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var ev input.Event
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				log.Printf("bad touch event from %s: %v", r.RemoteAddr, err)
				return
			}
			switch ev.Device {
			case "tablet":
				if ev.Type == "activate" {
					active := ev.Active != nil && *ev.Active
					tabletDev.SetActive(active)
				} else if err := tabletDev.ProcessEvent(ev); err != nil {
					log.Printf("tablet event error from %s: %v", r.RemoteAddr, err)
				}
			case "trackpad":
				if err := pad.ProcessEvent(ev); err != nil {
					log.Printf("trackpad event error from %s: %v", r.RemoteAddr, err)
				}
			default:
				log.Printf("unknown device %q from %s", ev.Device, r.RemoteAddr)
			}
		})
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		write(signalMsg{
			Type:             "candidate",
			Candidate:        init.Candidate,
			SDPMLineIndex:    init.SDPMLineIndex,
			SDPMid:           init.SDPMid,
			UsernameFragment: init.UsernameFragment,
		})
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("peer connection state for %s: %s", r.RemoteAddr, s.String())
		if s == webrtc.PeerConnectionStateConnected && videoStreamer != nil {
			videoStreamer.Start()
		}
	})

	// stopVideo stops the capture pipeline (if any) once for this connection.
	stopVideo := func() {
		if videoStreamer != nil {
			videoStreamer.Stop()
			videoStreamer = nil
		}
	}
	defer stopVideo()

	// When this client goes away, release any tool/contact state it left
	// behind. If a stroke/gesture was active when the data channel dropped or
	// the client backgrounded the page, its tip-up/pointerup was lost and the
	// virtual device would otherwise stay "down" — making the next client's
	// first touch invisible until it lifted and re-touched.
	defer tabletDev.Reset()
	defer pad.Reset()

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("websocket read error from %s: %v", r.RemoteAddr, err)
			}
			return
		}

		var msg signalMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("bad signal message from %s: %v", r.RemoteAddr, err)
			continue
		}

		switch msg.Type {
		case "offer":
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  msg.SDP,
			}); err != nil {
				log.Printf("setRemoteDescription failed: %v", err)
				continue
			}

			// If the offer requests video and video is enabled, build the
			// capture pipeline and add its track before answering. Failure
			// is non-fatal: we answer without a video track and the client
			// keeps using trackpad/tablet.
			if videoEnabled && hasVideoMedia(msg.SDP) && videoStreamer == nil {
				vs, err := video.New(video.Config{
					CardPath:  cli.VideoCard,
					MaxWidth:  cli.VideoWidth,
					FrameRate: cli.VideoFps,
					QP:        cli.VideoQP,
					LowPower:  cli.LowPower,
				})
				if err != nil {
					log.Printf("video unavailable for %s: %v", r.RemoteAddr, err)
					log.Printf("  if you do not need desktop video, run with --no-video to suppress this; if you do, make sure CAP_SYS_ADMIN is granted and a VAAPI-capable GPU is present")
				} else {
					if _, err := pc.AddTrack(vs.Track()); err != nil {
						log.Printf("add video track failed for %s: %v", r.RemoteAddr, err)
						vs.Stop()
					} else {
						videoStreamer = vs
						log.Printf("video track added for %s", r.RemoteAddr)
					}
				}
			}

			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Printf("createAnswer failed: %v", err)
				continue
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				log.Printf("setLocalDescription failed: %v", err)
				continue
			}
			write(signalMsg{Type: "answer", SDP: answer.SDP})

		case "candidate":
			if err := pc.AddICECandidate(webrtc.ICECandidateInit{
				Candidate:        msg.Candidate,
				SDPMLineIndex:    msg.SDPMLineIndex,
				SDPMid:           msg.SDPMid,
				UsernameFragment: msg.UsernameFragment,
			}); err != nil {
				log.Printf("addIceCandidate failed: %v", err)
			}

		default:
			log.Printf("unknown signal type from %s: %s", r.RemoteAddr, msg.Type)
		}
	}
}

func certDirectory() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "desktop_remote_mobile_companion")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

func loadOrGenerateCert(certFile, keyFile string) (tls.Certificate, string, error) {
	if _, err := os.Stat(certFile); err == nil {
		if _, err := os.Stat(keyFile); err == nil {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return tls.Certificate{}, "", err
			}
			fp, err := certFingerprint(certFile)
			if err == nil {
				log.Printf("certificate: loaded existing cert from %s (fingerprint %s)", certFile, fp)
			} else {
				log.Printf("certificate: loaded existing cert from %s (fingerprint unavailable: %v)", certFile, err)
			}
			return cert, fp, err
		}
		log.Printf("certificate: generating new self-signed cert (key file missing, cannot reuse %s)", certFile)
	} else {
		log.Printf("certificate: generating new self-signed cert (no existing cert found at %s)", certFile)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"desktop_remote_mobile_companion"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	for _, ip := range localIPs(false) {
		parsed := net.ParseIP(ip)
		if parsed != nil {
			template.IPAddresses = append(template.IPAddresses, parsed)
		}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, "", err
	}

	certOut, err := os.Create(certFile)
	if err != nil {
		return tls.Certificate{}, "", err
	}

	err = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err != nil {
		return tls.Certificate{}, "", err
	}
	err = certOut.Close()
	if err != nil {
		return tls.Certificate{}, "", err
	}

	keyOut, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	err = pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err != nil {
		return tls.Certificate{}, "", err
	}
	err = keyOut.Close()
	if err != nil {
		return tls.Certificate{}, "", err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("parse generated keypair: %w", err)
	}
	fp, err := certFingerprint(certFile)
	return cert, fp, err
}

func certFingerprint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("failed to decode certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:]), nil
}

// hasVideoMedia reports whether an SDP description contains a video media
// section (m=video). We use it to decide whether to build the capture
// pipeline before answering.
func hasVideoMedia(sdp string) bool {
	return strings.Contains(sdp, "m=video")
}

func localIPs(add_brackets_ipv6 bool) []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				s := ipnet.IP.String()
				if ipnet.IP.To4() == nil && add_brackets_ipv6 { // if ipv6
					if !strings.HasPrefix(s, "fe80") { // ignore link local
						out = append(out, "["+s+"]")
					}
				} else {
					out = append(out, s)
				}
			}
		}
	}
	return out
}
