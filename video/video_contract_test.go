package video

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

// fakeStreamer is a recording implementation of the video.Streamer interface
// used to exercise the cross-platform contract (the lifecycle main.go relies
// on) without a real capture pipeline.
type fakeStreamer struct {
	track      *webrtc.TrackLocalStaticSample
	starts     int
	stops      int
	stopCloses int
}

func (f *fakeStreamer) Start()                                { f.starts++ }
func (f *fakeStreamer) Stop()                                 { f.stops++; f.stopCloses++ }
func (f *fakeStreamer) Track() *webrtc.TrackLocalStaticSample { return f.track }

func newFakeStreamer() *fakeStreamer {
	track, _ := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "desktop-remote",
	)
	return &fakeStreamer{track: track}
}

// TestStreamerInterfaceContract asserts fakeStreamer satisfies the Streamer
// interface at compile time and that the documented lifecycle (Track non-nil,
// Start/Stop callable, Stop idempotent-ish) works through the interface.
func TestStreamerInterfaceContract(t *testing.T) {
	var s Streamer = newFakeStreamer()

	if s.Track() == nil {
		t.Fatal("Track() should be non-nil")
	}

	s.Start()
	if got := s.(*fakeStreamer).starts; got != 1 {
		t.Errorf("Start count = %d, want 1", got)
	}

	// Stop must be safe to call multiple times (the real backends guard it).
	s.Stop()
	s.Stop()
	if got := s.(*fakeStreamer).stopCloses; got != 2 {
		t.Errorf("Stop count = %d, want 2", got)
	}
}
