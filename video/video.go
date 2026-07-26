// Package video captures the desktop, encodes it to H264, and writes the
// samples to a Pion WebRTC track so they can be streamed to the mobile client.
package video

import (
	"github.com/pion/webrtc/v4"
)

// Config configures the capture pipeline. All fields are advisory; backends
// use the subset they support.
type Config struct {
	// CardPath is the DRM card to capture from (e.g. "/dev/dri/card1").
	// Empty means auto-detect the first /dev/dri/card* device.
	CardPath string

	// MaxWidth caps the output width; 0 means capture at native resolution.
	MaxWidth int

	// FrameRate is the capture/push frame rate in fps. 30 is a sensible
	// default; using the desktop's native frame rate is future work.
	FrameRate int

	// QP is the h264_vaapi constant-quality quantization parameter (0-52,
	// lower is higher quality). 24 is a good default.
	QP int

	// LowPower selects h264_vaapi low-power mode (0 = off, 1 = on). On means
	// the encoder uses the fixed-function encode engine, which is faster and
	// uses less GPU but may have slightly lower quality / fewer features.
	LowPower int
}

// Streamer owns a capture + encode pipeline and the Pion H264 track that
// receives the encoded samples.
type Streamer interface {
	// Start launches the capture/encode goroutine writing H264 samples to the
	// track returned by Track. It returns immediately. The goroutine runs
	// until Stop is called.
	Start()
	// Stop signals the capture goroutine to stop and frees all resources. It
	// is safe to call multiple times.
	Stop()
	// Track returns the H264 WebRTC track the encoded samples are written to.
	// It is non-nil for the lifetime of a successfully-constructed Streamer.
	Track() *webrtc.TrackLocalStaticSample
}

// New builds the capture + encode pipeline for the current platform and
// returns it as a Streamer, but does not start pushing frames yet. Call Start
// to begin streaming.
//
// A non-nil error means the system cannot capture (no VAAPI, no kmsgrab, no
// /dev/dri, or the platform has no implementation); the caller should continue
// without a video track.
func New(cfg Config) (Streamer, error) {
	return newStreamer(cfg)
}
