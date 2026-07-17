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
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

//go:embed static/*
var staticFS embed.FS

//go:embed VERSION
var version string

const uinputInstructions = `uinput access denied. To fix permissions, run:

  echo 'KERNEL=="uinput", MODE="0660", GROUP="input"' | sudo tee /etc/udev/rules.d/99-uinput.rules
  sudo udevadm control --reload
  sudo udevadm trigger

Then make sure your user is in the 'input' group:

  sudo usermod -aG input $USER

Log out and log back in for the group change to take effect.`

const (
	listenAddr = ":8081"
	certFile   = "server.crt"
	keyFile    = "server.key"
	staticDir  = "static"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var cli struct {
	Port int `arg:"-p,--port" default:"8080" help:"HTTPS listen port"`
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
		log.Fatalf("failed to register virtual trackpad: %v\n\n%s", err, uinputInstructions)
	}
	defer pad.Close()

	tabletDev, err := tablet.New()
	if err != nil {
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

	log.Printf("HTTPS listening on https://localhost%s", listenAddr)
	log.Printf(" certificate stored in %s", certDir)
	log.Printf(" certificate SHA-256 fingerprint: %s", fingerprint)
	for _, ip := range localIPs() {
		log.Printf(" also reachable at https://%s%s", ip, listenAddr)
	}

	server := &http.Server{
		Addr: listenAddr,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}
	log.Fatal(server.ListenAndServeTLS("", ""))
}

func signalHandler(w http.ResponseWriter, r *http.Request, pad *trackpad.Device, tabletDev *tablet.Device) {
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

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("data channel received from %s", r.RemoteAddr)
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			fmt.Printf("%s\n", string(msg.Data))

			var ev input.Event
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				log.Printf("bad touch event from %s: %v", r.RemoteAddr, err)
				return
			}
			switch ev.Device {
			case "tablet":
				if err := tabletDev.ProcessEvent(ev); err != nil {
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
	})

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
			return cert, fp, err
		}
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

	for _, ip := range localIPs() {
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
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certOut.Close()

	keyOut, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	keyOut.Close()

	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
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

func localIPs() []string {
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
				out = append(out, ipnet.IP.String())
			}
		}
	}
	return out
}
