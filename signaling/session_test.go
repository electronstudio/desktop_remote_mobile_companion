package signaling

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
	"github.com/electronstudio/desktop_remote_mobile_companion/video"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// fakeStreamer is a recording video.Streamer used to exercise the fail-open
// policy in maybeAddVideoTrack without a real capture pipeline. It mirrors
// video/video_contract_test.go's fakeStreamer.
type fakeStreamer struct {
	track  *webrtc.TrackLocalStaticSample
	starts int
	stops  int
}

func newFakeStreamer() *fakeStreamer {
	track, _ := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "desktop-remote",
	)
	return &fakeStreamer{track: track}
}

func (f *fakeStreamer) Start()                                { f.starts++ }
func (f *fakeStreamer) Stop()                                 { f.stops++ }
func (f *fakeStreamer) Track() *webrtc.TrackLocalStaticSample { return f.track }

// fakeProc is an input.EventProcessor that records the events it receives. It
// does NOT implement input.Activator, modeling the trackpad.
type fakeProc struct {
	processed []input.Event
	resets    int
}

func (f *fakeProc) ProcessEvent(ev input.Event) error {
	f.processed = append(f.processed, ev)
	return nil
}
func (f *fakeProc) Reset() { f.resets++ }

// fakeActivatorProc embeds fakeProc and adds SetActive, implementing
// input.Activator, modeling the tablet.
type fakeActivatorProc struct {
	fakeProc
	setActive []bool
}

func (f *fakeActivatorProc) SetActive(a bool) { f.setActive = append(f.setActive, a) }

// newTestSession builds a Session wired with a real (in-process) peer
// connection so methods that touch s.pc (AddTrack, SetRemoteDescription) work
// without any network. The pc is closed on test cleanup.
func newTestSession(t *testing.T, videoEnabled bool) *Session {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	return &Session{
		pc:           pc,
		remote:       "test",
		processors:   map[string]input.EventProcessor{},
		videoEnabled: videoEnabled,
		newVideo:     video.New,
	}
}

// videoOffer generates a real offer SDP containing a recvonly m=video line, so
// maybeAddVideoTrack's success path can SetRemoteDescription it and AddTrack a
// matching video sender.
func videoOffer(t *testing.T) webrtc.SessionDescription {
	t.Helper()
	offerer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer offerer.Close()
	if _, err := offerer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return offer
}

func mustRoute(t *testing.T, s *Session, ev input.Event) {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	s.handleDataMessage(b)
}

// TestHandleDataMessageRouting covers the data-channel event router: device
// dispatch, the "activate" control via input.Activator, the faithful
// fall-through of "activate" to ProcessEvent for non-Activator devices, and
// the safe handling of unknown devices and bad JSON.
func TestHandleDataMessageRouting(t *testing.T) {
	pad := &fakeProc{}
	tab := &fakeActivatorProc{}
	s := &Session{
		remote:     "test",
		processors: map[string]input.EventProcessor{"trackpad": pad, "tablet": tab},
	}

	// tablet pointer event → tablet.ProcessEvent only.
	mustRoute(t, s, input.Event{Device: "tablet", Type: "pointerdown", T: []input.Touch{{ID: 1, X: 0.5, Y: 0.5}}})
	if len(tab.processed) != 1 || tab.processed[0].Type != "pointerdown" {
		t.Errorf("tablet pointerdown: processed=%v", tab.processed)
	}
	if len(pad.processed) != 0 {
		t.Errorf("trackpad must not receive a tablet event: %v", pad.processed)
	}

	// trackpad pointer event → trackpad.ProcessEvent.
	mustRoute(t, s, input.Event{Device: "trackpad", Type: "pointermove"})
	if len(pad.processed) != 1 || pad.processed[0].Type != "pointermove" {
		t.Errorf("trackpad pointermove: processed=%v", pad.processed)
	}

	// tablet activate true → tablet.SetActive(true), NOT ProcessEvent.
	active := true
	mustRoute(t, s, input.Event{Device: "tablet", Type: "activate", Active: &active})
	if len(tab.setActive) != 1 || !tab.setActive[0] {
		t.Errorf("tablet activate=true: setActive=%v", tab.setActive)
	}
	if len(tab.processed) != 1 {
		t.Errorf("activate must not call ProcessEvent: processed=%v", tab.processed)
	}

	// tablet activate false → tablet.SetActive(false).
	active = false
	mustRoute(t, s, input.Event{Device: "tablet", Type: "activate", Active: &active})
	if len(tab.setActive) != 2 || tab.setActive[1] {
		t.Errorf("tablet activate=false: setActive=%v", tab.setActive)
	}

	// trackpad activate (no Activator) falls through to ProcessEvent,
	// preserving the original routing where only the tablet intercepted it.
	mustRoute(t, s, input.Event{Device: "trackpad", Type: "activate"})
	if len(pad.processed) != 2 || pad.processed[1].Type != "activate" {
		t.Errorf("trackpad activate fallthrough: processed=%v", pad.processed)
	}

	// unknown device: no panic, no routing.
	before := len(pad.processed) + len(tab.processed)
	s.handleDataMessage([]byte(`{"device":"foo","type":"pointerdown"}`))
	if got := len(pad.processed) + len(tab.processed); got != before {
		t.Errorf("unknown device must not route: pad=%v tab=%v", pad.processed, tab.processed)
	}

	// bad JSON: no panic, no routing.
	s.handleDataMessage([]byte("{not json"))
	if got := len(pad.processed) + len(tab.processed); got != before {
		t.Errorf("bad json must not route: pad=%v tab=%v", pad.processed, tab.processed)
	}
}

// TestHandleDataMessageNilProcessor reproduces the reported segfault: a
// device whose registration failed at startup leaves a nil interface in the
// route map, and the first event routed to it must be logged and dropped,
// not jump through a nil itab and crash the process.
func TestHandleDataMessageNilProcessor(t *testing.T) {
	s := &Session{
		remote:     "test",
		processors: map[string]input.EventProcessor{"trackpad": nil},
	}
	// pointer event and activate on the nil device: no panic.
	s.handleDataMessage([]byte(`{"device":"trackpad","type":"pointermove","w":1,"h":1,"t":[{"id":1,"x":0.5,"y":0.5}]}`))
	s.handleDataMessage([]byte(`{"device":"trackpad","type":"activate","active":true}`))
}

// TestMaybeAddVideoTrack covers the fail-open policy: every precondition
// missing or construction step failing returns nil and leaves the connection
// intact, while the happy path adds the track and stores the streamer.
func TestMaybeAddVideoTrack(t *testing.T) {
	// video disabled → nil, nothing constructed.
	s := newTestSession(t, false)
	if got := s.maybeAddVideoTrack("m=video 9 UDP/TLS/RTP/SAVPF 96\r\n"); got != nil {
		t.Errorf("disabled: expected nil, got %T", got)
	}

	// enabled but offer has no m=video → nil.
	s = newTestSession(t, true)
	if got := s.maybeAddVideoTrack("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"); got != nil {
		t.Errorf("no m=video: expected nil, got %T", got)
	}

	// factory error → fail-open nil, streamer not set.
	s = newTestSession(t, true)
	s.newVideo = func(video.Config) (video.Streamer, error) { return nil, errors.New("no vaapi") }
	if got := s.maybeAddVideoTrack("m=video\r\n"); got != nil {
		t.Errorf("factory error: expected nil, got %T", got)
	}
	if s.videoStreamer != nil {
		t.Errorf("factory error: streamer must not be set")
	}

	// idempotent: a streamer already exists → nil and newVideo not called.
	s = newTestSession(t, true)
	existing := newFakeStreamer()
	s.videoStreamer = existing
	s.newVideo = func(video.Config) (video.Streamer, error) {
		t.Fatal("newVideo must not be called when a streamer already exists")
		return nil, nil
	}
	if got := s.maybeAddVideoTrack("m=video\r\n"); got != nil {
		t.Errorf("idempotent: expected nil, got %T", got)
	}
	if s.videoStreamer != existing {
		t.Errorf("idempotent: existing streamer must not be replaced")
	}

	// success: enabled + m=video → returns the streamer, adds the track, sets
	// the field, and does not Stop it (Stop is for the failure path only).
	s = newTestSession(t, true)
	offer := videoOffer(t)
	if err := s.pc.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	fake := newFakeStreamer()
	s.newVideo = func(video.Config) (video.Streamer, error) { return fake, nil }
	if got := s.maybeAddVideoTrack(offer.SDP); got != fake {
		t.Fatalf("success: expected fake streamer, got %v", got)
	}
	if s.videoStreamer != fake {
		t.Errorf("success: s.videoStreamer must be set to the fake")
	}
	if fake.stops != 0 {
		t.Errorf("success: Stop must not be called, got %d", fake.stops)
	}
}

// TestCloseUnblocksRun confirms Close makes a running Session return from
// Run (the property server.Shutdown relies on, since http.Server.Shutdown
// does not close hijacked WebSocket connections) and that Close is safe to
// call twice.
func TestCloseUnblocksRun(t *testing.T) {
	upsgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := upsgrader.Upgrade(w, r, nil); err != nil {
			return
		}
		// Keep the server side open: the test only needs a live peer socket.
		select {}
	}))
	defer srv.Close()

	// The Session needs any live websocket: use the client end of the pair
	// (Close/runLoop treat it symmetrically).
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	s := New(Config{WS: conn, Remote: "test", Processors: map[string]input.EventProcessor{}})
	done := make(chan struct{})
	go func() {
		_ = s.Run()
		close(done)
	}()

	s.Close()
	s.Close() // second call must be a harmless no-op

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Close")
	}
}

// TestCloseWithoutWs confirms Close is safe on a Session that never got a
// WebSocket (e.g. constructed for unit tests).
func TestCloseWithoutWs(t *testing.T) {
	s := &Session{remote: "test"}
	s.Close()
}

// TestNewDefaultsNewVideo confirms New supplies video.New when Config.NewVideo
// is nil, so production callers do not have to set it.
func TestNewDefaultsNewVideo(t *testing.T) {
	s := New(Config{
		Processors: map[string]input.EventProcessor{},
	})
	if s.newVideo == nil {
		t.Fatal("New did not default newVideo to video.New")
	}
	if _, err := s.newVideo(video.Config{Source: "none"}); err == nil {
		t.Errorf("default newVideo should reject source=none, proving it is video.New")
	}
}
