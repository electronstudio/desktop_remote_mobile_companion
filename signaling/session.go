// Package signaling owns the WebRTC signaling protocol for a single client
// connection: the WebSocket signaling loop, the peer-connection lifecycle,
// data-channel event routing to virtual input devices, and the desktop-video
// capture pipeline (constructed on demand from the offer, fail-open).
//
// It is connection-scoped: each /signal WebSocket becomes one Session. All of
// its dependencies (the upgraded WebSocket, the device route table, and the
// video configuration) are passed in explicitly via Config, so the package
// does not read package-level globals and is unit-testable in isolation.
package signaling

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
	"github.com/electronstudio/desktop_remote_mobile_companion/video"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// signalMsg is a single message on the signaling WebSocket: an SDP offer,
// SDP answer, or a trickle ICE candidate.
type signalMsg struct {
	Type             string  `json:"type"` // offer | answer | candidate
	SDP              string  `json:"sdp,omitempty"`
	Candidate        string  `json:"candidate,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

// defaultRTCConfig is the WebRTC configuration shared by every session. It
// uses a single public STUN server for host-candidate gathering; local
// connections (the phone and the PC on the same LAN) do not need a TURN
// server.
var defaultRTCConfig = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	},
}

// Config carries the per-connection dependencies a Session needs. It is
// constructed once by main (the devices and video config never change between
// connections) and passed to New for each /signal request.
type Config struct {
	// WS is the already-upgraded WebSocket carrying signaling messages.
	WS *websocket.Conn
	// Remote is r.RemoteAddr, used only for log lines.
	Remote string
	// Processors routes data-channel events by Event.Device name to the
	// virtual input device that should handle them.
	Processors map[string]input.EventProcessor
	// VideoEnabled is the startup decision (from --video-source and the
	// CAP_SYS_ADMIN check) of whether to attempt desktop video at all.
	VideoEnabled bool
	// VideoConfig is the capture/encoder configuration passed to video.New
	// when an offer requests video.
	VideoConfig video.Config
	// NewVideo builds a Streamer from a Config. It defaults to video.New;
	// tests inject a fake to exercise the fail-open policy without hardware.
	NewVideo func(video.Config) (video.Streamer, error)
}

// Session is one client connection: a WebSocket signaling channel, the
// WebRTC peer connection it negotiates, the data-channel event router, and
// (optionally) a desktop-video capture pipeline.
type Session struct {
	ws     *websocket.Conn
	pc     *webrtc.PeerConnection
	remote string

	writeMu    sync.Mutex
	processors map[string]input.EventProcessor

	videoEnabled  bool
	videoCfg      video.Config
	newVideo      func(video.Config) (video.Streamer, error)
	videoStreamer video.Streamer
}

// New constructs a Session from cfg. It does not create the peer connection
// yet; Run does that so a construction failure is reported through Run's
// error return. NewVideo is defaulted to video.New when unset.
func New(cfg Config) *Session {
	s := &Session{
		ws:           cfg.WS,
		remote:       cfg.Remote,
		processors:   cfg.Processors,
		videoEnabled: cfg.VideoEnabled,
		videoCfg:     cfg.VideoConfig,
		newVideo:     cfg.NewVideo,
	}
	if s.newVideo == nil {
		s.newVideo = video.New
	}
	return s
}

// Run drives the whole connection: it creates the peer connection, wires the
// callbacks, and runs the signaling loop until the WebSocket closes. It logs
// the connection-lifecycle lines itself and returns an error only for the
// peer-connection creation failure (so the caller can log it consistently);
// all other failures are logged here and the loop continues or returns.
func (s *Session) Run() error {
	defer s.ws.Close()

	log.Printf("signal connection from %s", s.remote)

	pc, err := webrtc.NewPeerConnection(defaultRTCConfig)
	if err != nil {
		return fmt.Errorf("peer connection creation failed: %w", err)
	}
	s.pc = pc
	defer s.pc.Close()

	s.pc.OnDataChannel(s.onDataChannel)
	s.pc.OnICECandidate(s.onICECandidate)
	s.pc.OnConnectionStateChange(s.onConnectionStateChange)

	// Order matters: these run LIFO on return, so the device state is released
	// first (lift any stuck tip/contacts), then the video pipeline is stopped,
	// then the peer connection and WebSocket close. This matches the original
	// signalHandler defer order.
	defer s.stopVideo()
	defer s.resetProcessors()

	s.runLoop()
	return nil
}

// Close terminates the session by closing the WebSocket: the blocked
// ReadMessage in runLoop returns an error, Run's defer chain runs (device
// state reset, video pipeline stop, peer connection close) and Run returns.
// It is safe to call multiple times and from any goroutine; a second close
// of the WebSocket returns an error which is ignored here. It is used by
// server shutdown, which must close hijacked WebSocket connections itself
// because http.Server.Shutdown does not.
func (s *Session) Close() {
	if s.ws != nil {
		s.ws.Close()
	}
}

// runLoop reads signaling messages until the WebSocket closes or errors. The
// offer path is delegated to handleOffer (which returns a wrapped, staged
// error so a single log line names the failing step); candidates and unknown
// types are handled inline.
func (s *Session) runLoop() {
	for {
		_, data, err := s.ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("websocket read error from %s: %v", s.remote, err)
			}
			return
		}

		var msg signalMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("bad signal message from %s: %v", s.remote, err)
			continue
		}

		switch msg.Type {
		case "offer":
			if err := s.handleOffer(msg); err != nil {
				log.Printf("%v", err)
				continue
			}
		case "candidate":
			if err := s.pc.AddICECandidate(webrtc.ICECandidateInit{
				Candidate:        msg.Candidate,
				SDPMLineIndex:    msg.SDPMLineIndex,
				SDPMid:           msg.SDPMid,
				UsernameFragment: msg.UsernameFragment,
			}); err != nil {
				log.Printf("addIceCandidate failed: %v", err)
			}
		default:
			log.Printf("unknown signal type from %s: %s", s.remote, msg.Type)
		}
	}
}

// handleOffer applies a remote offer, optionally adds the desktop-video track,
// creates and sends the local answer. Each failing step returns a wrapped
// error whose message matches the original log line, so the loop's single
// log.Printf("%v", err) reproduces the prior output exactly.
func (s *Session) handleOffer(msg signalMsg) error {
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  msg.SDP,
	}); err != nil {
		return fmt.Errorf("setRemoteDescription failed: %w", err)
	}

	// Adding the video track before answering lets the answer advertise it.
	// Failure is non-fatal (maybeAddVideoTrack logs and returns nil): we
	// answer without a video track and the client keeps using trackpad/tablet.
	s.maybeAddVideoTrack(msg.SDP)

	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("createAnswer failed: %w", err)
	}
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("setLocalDescription failed: %w", err)
	}
	s.write(signalMsg{Type: "answer", SDP: answer.SDP})
	return nil
}

// maybeAddVideoTrack builds the desktop capture pipeline and adds its H264
// track to the peer connection when the offer requests video and video is
// enabled, then returns the streamer. It encapsulates the whole fail-open
// policy: any precondition missing or any construction step failing is logged
// and returns nil, leaving the connection without video but otherwise intact.
//
// The guard (video disabled, a streamer already exists, or no video media in
// the offer) is checked first so the rest of the function is a flat
// construct-then-attach sequence instead of a deeply nested else chain.
func (s *Session) maybeAddVideoTrack(offerSDP string) video.Streamer {
	if !s.videoEnabled || s.videoStreamer != nil || !hasVideoMedia(offerSDP) {
		return nil
	}

	vs, err := s.newVideo(s.videoCfg)
	if err != nil {
		log.Printf("video unavailable for %s: %v", s.remote, err)
		log.Printf("  if you do not need desktop video, run with --video-source=none to suppress this; if you do, make sure CAP_SYS_ADMIN is granted and a VAAPI-capable GPU is present")
		return nil
	}

	if _, err := s.pc.AddTrack(vs.Track()); err != nil {
		log.Printf("add video track failed for %s: %v", s.remote, err)
		vs.Stop()
		return nil
	}

	s.videoStreamer = vs
	log.Printf("video track added for %s", s.remote)
	return vs
}

// onDataChannel wires the client's "touch" data channel: each message is
// decoded and routed to the device handling Event.Device.
func (s *Session) onDataChannel(dc *webrtc.DataChannel) {
	log.Printf("data channel received from %s", s.remote)
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		s.handleDataMessage(msg.Data)
	})
}

// handleDataMessage decodes one data-channel message and routes the event.
// The "activate" control message is handled by devices that implement
// input.Activator (the tablet); a device that does not implement it leaves
// "activate" to ProcessEvent, preserving the original routing where only the
// tablet intercepted "activate" and every other device passed it through.
func (s *Session) handleDataMessage(data []byte) {
	var ev input.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		log.Printf("bad touch event from %s: %v", s.remote, err)
		return
	}

	proc, ok := s.processors[ev.Device]
	if !ok {
		log.Printf("unknown device %q from %s", ev.Device, s.remote)
		return
	}
	// A device whose registration failed at startup (e.g. /dev/uinput was
	// unavailable) leaves a nil interface in the route map; without this
	// guard the method calls below would jump through a nil itab and
	// segfault the whole process.
	if proc == nil {
		log.Printf("device %q unavailable for %s: virtual device registration failed at startup", ev.Device, s.remote)
		return
	}

	if ev.Type == "activate" {
		if act, ok := proc.(input.Activator); ok {
			act.SetActive(ev.Active != nil && *ev.Active)
			return
		}
	}

	if err := proc.ProcessEvent(ev); err != nil {
		log.Printf("%s event error from %s: %v", ev.Device, s.remote, err)
	}
}

// onICECandidate forwards each locally-gathered ICE candidate to the client
// as a trickle "candidate" message.
func (s *Session) onICECandidate(c *webrtc.ICECandidate) {
	if c == nil {
		return
	}
	init := c.ToJSON()
	s.write(signalMsg{
		Type:             "candidate",
		Candidate:        init.Candidate,
		SDPMLineIndex:    init.SDPMLineIndex,
		SDPMid:           init.SDPMid,
		UsernameFragment: init.UsernameFragment,
	})
}

// onConnectionStateChange logs state transitions and starts the video
// pipeline once the connection is fully established (ICE and DTLS connected),
// so the capture goroutine does not race ahead of the data channel setup.
func (s *Session) onConnectionStateChange(state webrtc.PeerConnectionState) {
	log.Printf("peer connection state for %s: %s", s.remote, state.String())
	if state == webrtc.PeerConnectionStateConnected && s.videoStreamer != nil {
		s.videoStreamer.Start()
	}
}

// write sends one signaling message to the WebSocket, serialized to JSON. The
// mutex makes concurrent sends safe (ICE candidates can arrive from a pion
// goroutine while the loop is writing an answer on the read goroutine).
func (s *Session) write(msg signalMsg) {
	b, _ := json.Marshal(msg)
	s.writeMu.Lock()
	s.ws.WriteMessage(websocket.TextMessage, b)
	s.writeMu.Unlock()
}

// stopVideo stops the capture pipeline for this connection once. It is safe to
// call when there is no pipeline (the common case when video is disabled or
// the offer did not request video).
func (s *Session) stopVideo() {
	if s.videoStreamer != nil {
		s.videoStreamer.Stop()
		s.videoStreamer = nil
	}
}

// resetProcessors releases any tool/contact state every device holds, so a
// disconnect mid-stroke does not leave the virtual device "down" for the next
// client. The devices are independent, so iteration order is immaterial.
func (s *Session) resetProcessors() {
	for _, p := range s.processors {
		p.Reset()
	}
}

// hasVideoMedia reports whether an SDP description contains a video media
// section (m=video). It is used to decide whether to build the capture
// pipeline before answering. (The substring check is fragile but matches the
// original behavior; a proper SDP parse is tracked separately.)
func hasVideoMedia(sdp string) bool {
	return strings.Contains(sdp, "m=video")
}
