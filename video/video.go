// Package video captures the desktop, encodes it to H264, and writes the
// samples to a Pion WebRTC track so they can be streamed to the mobile client.
package video

import (
	"fmt"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
)

// CaptureWidth and CaptureHeight hold the native capture resolution of the
// currently active video stream in pixels. They are set by a backend
// constructor (synchronously, before New returns) once the capture source is
// open (for pipewire: once the PipeWire format is negotiated), and reset to
// 0 when that stream stops. They are read/written
// atomically so other modules can read them concurrently with the capture
// goroutine. A value of 0 means no video stream is active (or its resolution
// has not yet been detected).
//
// This is the capture (input) resolution — the size kmsgrab/x11grab/ddagrab
// read from the desktop. The streamed (encoder output) resolution is the
// same unless --video-width (Config.MaxWidth) caps it; that smaller size is
// only known lazily on the first frame, so this global reports the capture
// size.
//
// One client at a time is active (kmsgrab is exclusive; a second ddagrab
// streamer would also contend over the single D3D11 desktop duplication
// session), so a single global is sufficient; see AGENTS.md "One client at
// a time".
var (
	CaptureWidth  atomic.Int64
	CaptureHeight atomic.Int64
)

// Config configures the capture pipeline. All fields are advisory; backends
// use the subset they support.
type Config struct {
	// Source selects the capture backend. "kmsgrab" (the default, "") uses the
	// Linux kmsgrab DRM pipeline; "x11grab" captures the X server; "pipewire"
	// captures via xdg-desktop-portal + PipeWire (Wayland-native, no
	// CAP_SYS_ADMIN; the user confirms a share dialog every run); "none"
	// disables video and is handled by the caller (New is never called with
	// it). On Windows the source is always ddagrab regardless of this value.
	// Unknown values are rejected by the platform newStreamer with an error.
	Source string

	// Encoder selects the H264 encoder family. "" or "auto" (the default)
	// resolves on Linux to h264_nvenc on NVIDIA systems (except kmsgrab,
	// which cannot feed nvenc), else h264_vaapi if available, else libx264;
	// on Windows auto resolves to libx264 (no GPU vendor detection is done
	// there; that is a documented possible future improvement). The explicit
	// values "vaapi", "nvenc", and "libx264" are always accepted, plus
	// "amf" and "mf" on Windows.
	Encoder string

	// CardPath is the DRM card to capture from (e.g. "/dev/dri/card1").
	// Empty means auto-detect the first /dev/dri/card* device. Unused by the
	// Windows ddagrab backend (ddagrab always captures the primary display).
	CardPath string

	// MaxWidth caps the output width; 0 means capture at native resolution.
	// (Currently only plumbed through the Linux backends; ddagrab ignores it.)
	MaxWidth int

	// FrameRate is the capture/push frame rate in fps. 30 is a sensible
	// default; using the desktop's native frame rate is future work.
	FrameRate int

	// QP is the encoder quality setting (h264_vaapi/h264_nvenc QP and
	// libx264 CRF: 0-52, lower is higher quality; h264_amf CQP QP; mapped
	// to h264_mf's 0-100 quality property as 100-2*QP). 24 is a good
	// default.
	QP int

	// LowPower selects h264_vaapi low-power mode (0 = off, 1 = on). On means
	// the encoder uses the fixed-function encode engine, which is faster and
	// uses less GPU but may have slightly lower quality / fewer features.
	LowPower bool
}

// Streamer owns a capture + encode pipeline and the Pion H264 track that
// receives the encoded samples.
type Streamer interface {
	// Start launches the capture/encode goroutine writing H264 samples to the
	// track returned by Track. It returns immediately. The goroutine runs
	// until Stop is called. Start is idempotent: only the first call starts
	// the pipeline (the WebRTC session calls Start on every Connected state
	// transition, which can fire repeatedly after an ICE restart), and a
	// call after Stop is a no-op.
	Start()
	// Stop signals the capture goroutine to stop and frees all resources. It
	// is safe to call multiple times and safe to call before Start: a
	// pipeline that was never started has no goroutine to wait for.
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
	if cfg.Source == "none" {
		return nil, fmt.Errorf("video: source %q disables video; the caller should not call New", cfg.Source)
	}
	// Source and encoder validation/dispatch is platform-specific: Linux
	// routes kmsgrab/x11grab to their backends (rejecting unknown sources);
	// Windows always uses ddagrab regardless of the requested source.
	return newStreamer(cfg)
}
