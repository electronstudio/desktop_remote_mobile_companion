package server

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
	"encoding/pem"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/alexflint/go-arg"
	"github.com/electronstudio/desktop_remote_mobile_companion/input"
	"github.com/electronstudio/desktop_remote_mobile_companion/signaling"
	"github.com/electronstudio/desktop_remote_mobile_companion/tablet"
	"github.com/electronstudio/desktop_remote_mobile_companion/trackpad"
	"github.com/electronstudio/desktop_remote_mobile_companion/video"
	"github.com/electronstudio/low_latency_dictation/toast"
	"github.com/fatih/color"
	"github.com/gorilla/websocket"
	"github.com/mdp/qrterminal/v3"
)

//go:embed static/*
var staticFS embed.FS

//go:embed VERSION
var version string

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type CLI struct {
	Port int `arg:"-p,--port" default:"8080" help:"HTTPS listen port"`

	VideoSource    string `arg:"--video-source" default:"kmsgrab" help:"desktop video capture source: \"kmsgrab\" (DRM, default), \"x11grab\" (X server), or \"none\" to disable video; ignored on Windows, where the source is always ddagrab"`
	VideoEncoder   string `arg:"--video-encoder" default:"auto" help:"video H264 encoder (choices listed below)"`
	VideoCard      string `arg:"--video-card" default:"" help:"DRM card to capture (e.g. /dev/dri/card1); empty auto-detects; ignored on Windows (ddagrab captures the primary display)"`
	VideoFps       int    `arg:"--video-fps" default:"30" help:"video capture frame rate"`
	VideoQP        int    `arg:"--video-qp" default:"24" help:"encoder quality (h264_vaapi/nvenc QP, h264_amf CQP QP, or libx264 CRF; lower is higher quality; mapped to h264_mf's 0-100 quality property)"`
	VideoWidth     int    `arg:"--video-width" default:"0" help:"cap video output width; 0 native"`
	VideoIntelFast bool   `arg:"--video-intel-fast" default:"false" help:"enable h264_vaapi low-power mode; ignored for other encoders"`
	DontGrabMouse  bool   `arg:"--dont-grab-mouse" default:"false" help:"disable the tablet hover keep-alive (Mutter cooldown workaround); use on compositors without the cooldown (e.g. wlroots) so the mouse is not grabbed while idle"`
	DontRunSudo    bool   `arg:"--dont-run-sudo" default:"false" help:"do not try to gain privileges with sudo"`
}

// CLIDefaults returns a CLI populated with the go-arg `default:` struct-tag
// values (no command-line parsing), so callers that bypass go-arg — e.g. the
// inara_gui, which has no CLI flags — can still take their starting
// configuration from the same defaults the CLI binary uses. It parses an
// empty argument list, so every field gets exactly its tag default.
func CLIDefaults() CLI {
	var cli CLI
	p, err := arg.NewParser(arg.Config{Program: "inara"}, &cli)
	if err != nil {
		panic(err) // only possible if the CLI struct tags are malformed
	}
	if err := p.Parse(nil); err != nil {
		panic(err) // an empty arg list can only fail on a malformed default tag
	}
	return cli
}

// Epilogue implements go-arg's Epilogued interface, so the parser appends
// this text to the bottom of --help. The --video-encoder help text in the
// struct tag above must stay short because struct tags are compile-time
// constants and cannot vary by platform; here we are free to call
// video.EncoderLabels(), which is build-tag-selected per platform and lists
// exactly the encoders available where the binary is running.
func (CLI) Epilogue() string {
	labels := video.EncoderLabels()
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("--video-encoder choices on this platform:\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s\n", labels[k])
	}
	// auto is not part of EncoderLabels (it is not a concrete encoder); its
	// resolution is platform-specific, which is safe to branch on here
	// because this text is generated at runtime on the target platform.
	if runtime.GOOS == "windows" {
		b.WriteString("  auto (mf)")
	} else {
		b.WriteString("  auto (nvenc on NVIDIA, else vaapi, else libx264)")
	}
	return b.String()
}

// Compile-time checks that the concrete device interfaces satisfy the
// routing contracts the signaling layer relies on: both devices are event
// processors, and only the tablet additionally handles the "activate"
// control (input.Activator).
var (
	_ input.EventProcessor = (trackpad.Device)(nil)
	_ input.EventProcessor = (tablet.Device)(nil)
	_ input.Activator      = (tablet.Device)(nil)
)

func Run(cli CLI) {
	listenAddr := fmt.Sprintf(":%d", cli.Port)
	versionStr := strings.TrimSpace(version)

	if err := toast.Init(nil); err != nil {
		log.Printf("warning: %v\n", err)
	}

	// videoEnabled is the effective decision about whether desktop video
	// streaming will be attempted. It starts true unless the user selected
	// --video-source=none, and may be cleared at startup if the process lacks
	// CAP_SYS_ADMIN, which kmsgrab needs to acquire DRM master and map the
	// framebuffer. It is a local passed to each signaling session via Config,
	// so the CAP_SYS_ADMIN check below only has to clear it once.
	videoEnabled := cli.VideoSource != "none"

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
		_ = toast.Show("failed to register virtual trackpad", uinputInstructions, true)
		color.Set(color.FgYellow)
		log.Printf("failed to register virtual trackpad: %v\n\n%s", err, uinputInstructions)
		color.Unset()
		reExecWithSudo()
	}
	defer pad.Close()

	tabletDev, err := tablet.New(!cli.DontGrabMouse)
	if err != nil {
		_ = toast.Show("failed to register virtual graphics tablet", uinputInstructions, true)
		color.Set(color.FgYellow)
		log.Printf("failed to register virtual graphics tablet: %v\n\n%s", err, uinputInstructions)
		color.Unset()
		reExecWithSudo()
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

	// videoCfg is built once and reused for every connection's capture
	// pipeline; it never changes between offers.
	videoCfg := video.Config{
		Source:    cli.VideoSource,
		Encoder:   cli.VideoEncoder,
		CardPath:  cli.VideoCard,
		MaxWidth:  cli.VideoWidth,
		FrameRate: cli.VideoFps,
		QP:        cli.VideoQP,
		LowPower:  cli.VideoIntelFast,
	}

	// processors routes data-channel events to the virtual input device that
	// handles each Event.Device name, keeping the signaling layer
	// device-agnostic (see input.EventProcessor / input.Activator).
	processors := map[string]input.EventProcessor{
		"trackpad": pad,
		"tablet":   tabletDev,
	}

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
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade failed: %v", err)
			return
		}
		if err := signaling.New(signaling.Config{
			WS:           ws,
			Remote:       r.RemoteAddr,
			Processors:   processors,
			VideoEnabled: videoEnabled,
			VideoConfig:  videoCfg,
		}).Run(); err != nil {
			log.Printf("%v", err)
		}
	})

	if cli.VideoSource == "kmsgrab" {
		// kmsgrab requires CAP_SYS_ADMIN. Without it every phone connection
		// would fail with a cryptic "No handle set on framebuffer" / EINVAL
		// error from FFmpeg. Detect that once up front, tell the user how to
		// fix it, and silently disable video for this run so trackpad/tablet
		// keep working without per-connection noise.
		if ok, err := hasCapSysAdmin(); err != nil {
			log.Printf("warning: could not check CAP_SYS_ADMIN (%v); desktop video may fail", err)
		} else if !ok {
			_ = toast.Show("error: missing permissions", videoMissingCapInstructions, false)
			color.Set(color.FgRed)
			log.Printf("error: desktop video streaming: the process lacks CAP_SYS_ADMIN, which kmsgrab needs to capture the framebuffer.")
			// File capabilities (setcap) are the preferred fix, but they are
			// silently ignored on nosuid mounts, so give the right advice for
			// the actual situation instead of sending the user down a blind
			// alley. sudo works regardless (root has the cap in its bounding set).
			color.Set(color.FgYellow)
			if nosuid, ferr := onNoSuidMount(); ferr != nil {
				log.Printf("note: could not determine whether the executable is on a nosuid mount (%v)", ferr)
				log.Print(videoMissingCapInstructions)
			} else if nosuid {
				log.Print(videoMissingCapOnNoSuidMountInstructions)
			} else {
				log.Print(videoMissingCapInstructions)
			}
			color.Unset()
			if !cli.DontRunSudo {
				reExecWithSudo()
			}
			videoEnabled = false
		}
	}

	if runtime.GOOS != "windows" && video.NvidiaGPU() && cli.VideoSource == "kmsgrab" {
		// kmsgrab decodes DRM frames and feeds them to VAAPI via hwmap. NVIDIA
		// systems typically have no VAAPI, so the kmsgrab pipeline usually
		// cannot capture there; x11grab (or, on Windows, nvenc with ddagrab) is
		// the right choice. Warn but proceed: the per-connection attempt will
		// fail gracefully if VAAPI really is missing.
		log.Printf("warning: --video-source=kmsgrab on an NVIDIA system: kmsgrab relies on VAAPI, which is usually unavailable on NVIDIA, so desktop video may fail. Consider --video-source x11grab.")
		_ = toast.Show("warning: nvidia detected", "Probably won't work with kmsgrab/Wayland.  Use --video-source x11grab.", false)
	}
	if runtime.GOOS != "windows" && isWaylandSession() && cli.VideoSource == "x11grab" {
		// x11grab talks to the X server (XWayland under a Wayland session), so
		// it can only capture X11/XWayland content; native Wayland surfaces are
		// invisible to it and may appear as a black screen. kmsgrab captures
		// the DRM framebuffer directly and is the better choice on Wayland.
		log.Printf("warning: --video-source x11grab on a Wayland session: x11grab captures the X server (XWayland), so it will likely capture only X11 windows or a black screen rather than native Wayland surfaces. Consider --video-source kmsgrab.")
		_ = toast.Show("warning: x11grab on a Wayland session", "Use --video-source kmsgrab", false)
	}

	if videoEnabled {
		enc := cli.VideoEncoder
		if enc == "" {
			enc = "auto"
		}
		source := cli.VideoSource
		if runtime.GOOS == "windows" {
			// The source flag is ignored on Windows; log the actual backend so
			// the log doesn't claim kmsgrab (the tag default) is in use.
			source = "ddagrab"
		}
		log.Printf("desktop video streaming enabled (source=%s, encoder=%s)", source, enc)
		if cli.VideoSource == "kmsgrab" {
			if cli.VideoCard != "" {
				log.Printf("  capture card: %s", cli.VideoCard)
			} else {
				log.Printf("  capture card: auto-detect")
			}
		}
		log.Printf("  fps=%d qp=%d low-power=%t", cli.VideoFps, cli.VideoQP, cli.VideoIntelFast)
	} else if cli.VideoSource == "none" {
		log.Printf("desktop video streaming disabled (--video-source=none)")
	} else {
		log.Printf("desktop video streaming disabled (missing CAP_SYS_ADMIN)")
	}

	//err = dropSudoPrivileges()
	//if err != nil {
	//	log.Printf("unable to drop sudo privileges: %v\n", err)
	//}

	log.Printf("HTTPS listening on https://localhost%s", listenAddr)
	log.Printf(" certificate stored in %s", certDir)
	log.Printf(" certificate SHA-256 fingerprint: %s", fingerprint)

	for _, ip := range LocalIPs(true) {
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

func certDirectory() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "inara")
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
			Organization: []string{"inara"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	for _, ip := range LocalIPs(false) {
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

// isWaylandSession reports whether the current graphical session is Wayland.
// x11grab only sees the X server (XWayland under Wayland), so on a Wayland
// session it typically captures only X11 windows or a black screen; we warn
// the user in that case. kmsgrab is unaffected (it reads the DRM framebuffer
// directly).
func isWaylandSession() bool {
	if t := os.Getenv("XDG_SESSION_TYPE"); t == "wayland" {
		return true
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return true
	}
	return false
}

func LocalIPs(add_brackets_ipv6 bool) []string {
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
