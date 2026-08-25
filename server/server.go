package server

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	ListenAddress string `arg:"--listen-address" default:"" help:"IP address for the HTTP server to listen on; empty listens on all interfaces (the default)"`

	VideoSource    string `arg:"--video-source" default:"kmsgrab" help:"desktop video capture source: \"kmsgrab\" (DRM, default), \"x11grab\" (X server), or \"none\" to disable video; ignored on Windows, where the source is always ddagrab"`
	VideoEncoder   string `arg:"--video-encoder" default:"auto" help:"video H264 encoder (choices listed below)"`
	VideoCard      string `arg:"--video-card" default:"" help:"DRM card to capture (e.g. /dev/dri/card1); empty auto-detects; ignored on Windows (ddagrab captures the primary display)"`
	VideoFps       int    `arg:"--video-fps" default:"30" help:"video capture frame rate"`
	VideoQP        int    `arg:"--video-qp" default:"24" help:"encoder quality (h264_vaapi/nvenc QP, h264_amf CQP QP, or libx264 CRF; lower is higher quality; mapped to h264_mf's 0-100 quality property)"`
	VideoWidth     int    `arg:"--video-width" default:"0" help:"cap video output width; 0 native"`
	VideoIntelFast bool   `arg:"--video-intel-fast" default:"false" help:"enable h264_vaapi low-power mode; ignored for other encoders"`
	DontGrabMouse  bool   `arg:"--dont-grab-mouse" default:"false" help:"disable the tablet hover keep-alive (Mutter cooldown workaround); use on compositors without the cooldown (e.g. wlroots) so the mouse is not grabbed while idle"`
	DontRunSudo    bool   `arg:"--dont-run-sudo" default:"false" help:"do not try to gain privileges with sudo"`
	Passcode       string `arg:"--passcode,env:INARA_PASSCODE" help:"passphrase clients must enter to connect; also read from $INARA_PASSCODE (the flag overrides the env var); empty disables authentication"`
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

// Server is one lifecycle of the inara server: construct it with New, serve
// with Run (which blocks), and stop it with Shutdown from another goroutine
// (a signal handler or the GUI Stop button). A Server is single-use: after
// Shutdown it cannot be restarted; construct a fresh one with New instead.
type Server struct {
	cli           CLI
	httpServer    *http.Server
	certDir       string
	caFingerprint string
	caCertPEM     []byte
	pad           trackpad.Device
	tablet        tablet.Device

	// sessions tracks the active signaling sessions so Shutdown can close
	// them explicitly: http.Server.Shutdown neither closes nor waits for
	// hijacked connections such as WebSockets. Closing a session makes its
	// Run loop return, which runs its defer chain (device state reset, video
	// pipeline stop -> clean avformat_close_input of the FFmpeg capture,
	// peer connection close). Killing the process mid-capture instead of
	// running this chain can crash the Wayland compositor.
	sessionsMu   sync.Mutex
	sessions     map[*signaling.Session]struct{}
	sessionsWG   sync.WaitGroup
	shuttingDown bool

	shutdownOnce sync.Once
	shutdownErr  error
}

// New builds a ready-to-serve Server: certificates, virtual input devices,
// HTTP handlers, and the startup capability checks. It does not listen yet;
// call Run for that. Setup errors are returned rather than exiting so
// callers (e.g. the GUI) can report them and stay alive.
func New(cli CLI) (*Server, error) {
	listenAddr := fmt.Sprintf(":%d", cli.Port)
	if cli.ListenAddress != "" {
		// JoinHostPort brackets IPv6 addresses; an empty address keeps the
		// previous ":port" form, which makes net/http listen on all
		// interfaces exactly as before.
		listenAddr = net.JoinHostPort(cli.ListenAddress, strconv.Itoa(cli.Port))
	}
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

	auth := newAuthGate(cli.Passcode)

	log.Printf("Desktop Remote Mobile Companion v%s", versionStr)
	if auth.enabled() {
		log.Printf("passcode authentication enabled") // the passcode itself is never logged
	}

	certDir, err := certDirectory()
	if err != nil {
		return nil, fmt.Errorf("certificate directory failed: %w", err)
	}
	certPath := filepath.Join(certDir, "server.crt")
	keyPath := filepath.Join(certDir, "server.key")

	// Clients install the local CA into their trust store once; the server
	// leaf it signs is reused across runs while still valid so clients that
	// click through the browser warning (no CA installed) keep their
	// exception; it is regenerated only when IPs/validity/CA change.
	caCert, caKey, caPEM, err := loadOrGenerateCA(certDir)
	if err != nil {
		return nil, fmt.Errorf("CA certificate setup failed: %w", err)
	}
	fingerprint := fingerprintOf(caCert)
	cert, err := loadOrGenerateLeaf(caCert, caKey, certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("certificate setup failed: %w", err)
	}

	pad, err := trackpad.New()
	if err != nil {
		_ = toast.Show("failed to register virtual trackpad", uinputInstructions, true)
		color.Set(color.FgYellow)
		log.Printf("failed to register virtual trackpad: %v\n\n%s", err, uinputInstructions)
		color.Unset()
		reExecWithSudo(cli)
	}

	tabletDev, err := tablet.New(!cli.DontGrabMouse)
	if err != nil {
		_ = toast.Show("failed to register virtual graphics tablet", uinputInstructions, true)
		color.Set(color.FgYellow)
		log.Printf("failed to register virtual graphics tablet: %v\n\n%s", err, uinputInstructions)
		color.Unset()
		reExecWithSudo(cli)
	}

	// The Server struct exists before the handlers below so the /signal
	// closure can register sessions in it; the remaining fields are filled
	// in at the end of New.
	s := &Server{
		cli:      cli,
		pad:      pad,
		tablet:   tabletDev,
		sessions: make(map[*signaling.Session]struct{}),
	}

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("static filesystem failed: %w", err)
	}

	indexBytes, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded index.html: %w", err)
	}
	// The page title shows the machine hostname so a user with several
	// servers can tell their browser tabs / installed web apps apart.
	titleStr := "Inara"
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		titleStr = "Inara " + hostname
	}
	// The CA fingerprint is injected so the in-app certificate-install help
	// can show the exact value the OS displays during installation, letting
	// the user confirm they are installing this server's CA.
	indexHTML := strings.NewReplacer(
		"{{VERSION}}", versionStr,
		"{{TITLE}}", html.EscapeString(titleStr),
		"{{CA_FINGERPRINT}}", fingerprint,
	).Replace(string(indexBytes))
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

	// A dedicated mux instead of http.DefaultServeMux: a Server is single-use
	// and the GUI may construct several over the app's lifetime, and double
	// registration on the default mux would panic.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Write([]byte(indexHTML))
			return
		}
		fileServer.ServeHTTP(w, r)
	})
	// /auth answers passcode-status queries (GET) and passcode login (POST).
	// It is registered unconditionally: with no passcode configured it simply
	// reports required=false, keeping the client logic uniform.
	mux.Handle("/auth", auth)
	// /ca.crt serves the local CA certificate so clients can install it
	// into their trust store (removing the self-signed-cert warning and
	// enabling PWA install / secure-context WebRTC). It is deliberately
	// unauthenticated even with a passcode set: it is a public key, not a
	// secret, and the first-time user needs it before reliably entering a
	// passcode. Only the public cert leaves the process; ca.key never does.
	// The application/x-x509-ca-cert MIME type is what Android's
	// Certificate Installer hooks when a downloaded .crt is opened; iOS
	// Safari instead triggers its configuration-profile flow from the
	// certificate content itself.
	serveCA := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", "attachment; filename=\"inara-ca.crt\"")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(s.caCertPEM)
	}
	mux.HandleFunc("/ca.crt", serveCA)
	mux.HandleFunc("/ca.pem", serveCA)
	// /version reports the running binary's embedded version so a client
	// can detect that the server was updated while its page was open (or
	// while a stale copy of the page survived in a cache) and reload to
	// pick up the new assets. It is deliberately unauthenticated, like
	// /ca.crt: the version is already displayed in the page footer to any
	// visitor, and a stale client must be able to check before entering a
	// passcode / behind an auth-cookie expiry.
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `{"version":%q}`+"\n", versionStr)
	})
	mux.HandleFunc("/signal", func(w http.ResponseWriter, r *http.Request) {
		// The browser attaches the session cookie to the WebSocket handshake
		// automatically. Gating the handshake here also gates the WebRTC data
		// channel, because no peer connection can be established without
		// exchanging SDP through this endpoint.
		if !auth.check(r) {
			log.Printf("rejected unauthenticated signaling attempt from %s", r.RemoteAddr)
			http.Error(w, "authentication required", http.StatusForbidden)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade failed: %v", err)
			return
		}
		sess := signaling.New(signaling.Config{
			WS:           ws,
			Remote:       r.RemoteAddr,
			Processors:   processors,
			VideoEnabled: videoEnabled,
			VideoConfig:  videoCfg,
		})
		if !s.addSession(sess) {
			// Shutting down: reject the connection instead of starting a new
			// session that Shutdown has already walked past.
			ws.Close()
			return
		}
		defer s.removeSession(sess)
		if err := sess.Run(); err != nil {
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
			reExecWithSudo(cli)
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

	s.certDir = certDir
	s.caFingerprint = fingerprint
	s.caCertPEM = caPEM
	s.httpServer = &http.Server{
		Addr:    listenAddr,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}
	return s, nil
}

// Run serves HTTPS on the address and certificate prepared by New. It blocks
// until the listener fails or Shutdown is called: a shutdown-induced stop
// returns nil, a listener failure returns the error. Call it at most once.
func (s *Server) Run() error {
	if s.cli.ListenAddress != "" {
		log.Printf("HTTPS listening on https://%s:%d", s.cli.ListenAddress, s.cli.Port)
	} else {
		log.Printf("HTTPS listening on https://localhost%s", s.httpServer.Addr)
	}
	log.Printf(" certificates stored in %s", s.certDir)
	log.Printf(" CA certificate SHA-256 fingerprint: %s", s.caFingerprint)
	log.Printf(" install the CA on client devices from /ca.crt to remove the certificate warning")

	ips := LocalIPs(true)
	if s.cli.ListenAddress != "" {
		// Bound to a single address: advertising the other interface IPs
		// would be misleading.
		ips = []string{s.cli.ListenAddress}
	}
	for _, ip := range ips {
		url := fmt.Sprintf("https://%s:%d", ip, s.cli.Port)
		if s.cli.ListenAddress == "" {
			log.Printf(" also reachable at %s", url)
		} else {
			log.Printf(" reachable at %s", url)
		}

		qrterminal.GenerateHalfBlock(url, qrterminal.L, os.Stdout)
	}

	if err := s.httpServer.ListenAndServeTLS("", ""); errors.Is(err, http.ErrServerClosed) {
		return nil
	} else {
		return err
	}
}

// Shutdown gracefully stops the server. The order matters: first every active
// signaling session is closed so each session's defer chain stops its video
// capture pipeline (a clean avformat_close_input of the FFmpeg kmsgrab input
// -- tearing the process down mid-capture can crash the Wayland compositor),
// then the HTTP server is shut down, then the virtual input devices are
// closed. It is idempotent and safe to call concurrently; later calls wait
// for the first to finish and return its error. If ctx has no deadline, a
// 5-second cap keeps a stuck session or capture from hanging the process.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		var errs []error
		log.Printf("shutting down gracefully")

		if ctx == nil {
			ctx = context.Background()
		}
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
		}

		s.sessionsMu.Lock()
		s.shuttingDown = true
		sessions := make([]*signaling.Session, 0, len(s.sessions))
		for sess := range s.sessions {
			sessions = append(sessions, sess)
		}
		s.sessionsMu.Unlock()

		for _, sess := range sessions {
			sess.Close()
		}

		waitDone := make(chan struct{})
		go func() { s.sessionsWG.Wait(); close(waitDone) }()
		select {
		case <-waitDone:
		case <-ctx.Done():
			log.Printf("shutdown: timed out waiting for %d session(s) to close", len(sessions))
			errs = append(errs, fmt.Errorf("timed out waiting for %d session(s) to close", len(sessions)))
		}

		if err := s.httpServer.Shutdown(ctx); err != nil {
			// The context may already be spent waiting on sessions; if the
			// listener stays open Run never unblocks and the process cannot
			// exit, so force-close: the long-lived (hijacked) connections
			// were already handled above.
			log.Printf("shutdown: %v; forcing listener close", err)
			errs = append(errs, err)
			if cerr := s.httpServer.Close(); cerr != nil {
				errs = append(errs, cerr)
			}
		}
		if s.pad != nil {
			errs = append(errs, s.pad.Close())
		}
		if s.tablet != nil {
			errs = append(errs, s.tablet.Close())
		}
		s.shutdownErr = errors.Join(errs...)
	})
	return s.shutdownErr
}

// addSession registers a new signaling session in the shutdown registry. It
// returns false when the server is shutting down, in which case the caller
// must reject the connection (the session is not registered and the WaitGroup
// is untouched). The registry lets Shutdown close hijacked WebSocket
// connections itself, which http.Server.Shutdown does not do.
func (s *Server) addSession(sess *signaling.Session) bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if s.shuttingDown {
		return false
	}
	s.sessions[sess] = struct{}{}
	s.sessionsWG.Add(1)
	return true
}

// removeSession unregisters a session whose Run has returned.
func (s *Server) removeSession(sess *signaling.Session) {
	s.sessionsMu.Lock()
	delete(s.sessions, sess)
	s.sessionsMu.Unlock()
	s.sessionsWG.Done()
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
