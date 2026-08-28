//go:build linux

// Package video captures the Wayland desktop via xdg-desktop-portal +
// PipeWire and feeds the raw frames to the shared H264 encoder (see
// encoder.go). This is the Wayland-native counterpart to the kmsgrab and
// x11grab backends: it needs neither CAP_SYS_ADMIN nor an X server, which
// makes it the right source on Wayland compositors where kmsgrab cannot run
// (e.g. NVIDIA systems without VAAPI) and x11grab only sees XWayland.
//
// The pipeline has three pieces:
//
//   - portal_linux.go (pure Go, godbus): the xdg-desktop-portal ScreenCast
//     consent flow. It returns the PipeWire node id and a connected PipeWire
//     remote fd. The desktop environment shows its share dialog on every
//     run (permissions are deliberately not persisted).
//   - pipewire_linux.c + pipewire_cgo_linux.go: a libpipewire client stream
//     connected to the portal's remote/node, negotiating raw 32-bit RGB
//     frames over shared memory and copying them into a bounded
//     (drop-oldest) queue.
//   - this file: the Streamer. Frames are software frames, exactly like
//     x11grab, so the encoder axis is unchanged: libx264 encodes them
//     directly, while vaapi/nvenc get them via hwupload.
//
// Capture is damage-driven: a static desktop produces no frames (like OBS's
// PipeWire capture), so the stream simply idles instead of spinning.
//
// If New returns an error (no portal, user cancelled the dialog, no
// PipeWire, or the chosen encoder is unavailable), the caller should simply
// continue without a video track; trackpad/tablet keep working.

package video

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// pipewireStreamer owns the portal session, the PipeWire capture stream and
// the shared encoder. It implements the video.Streamer interface.
type pipewireStreamer struct {
	cfg   Config
	track *webrtc.TrackLocalStaticSample
	enc   *encoder

	portal  *portalSession
	capture *pwCapture

	stop chan struct{}
	done chan struct{}
	pts  int64

	// mu guards started/stopped so Start and Stop are safe in any order and
	// from concurrent callers. The WebRTC session calls Start on every
	// PeerConnectionStateConnected transition, which can fire more than
	// once per connection (e.g. after a transient drop/ICE restart):
	// spawning a second captureLoop would double-close done (panic: close
	// of closed channel) and race two goroutines on the FFmpeg contexts.
	// Conversely Stop before Start (AddTrack failure, or a client that
	// disconnects before Connected) must not block on done — no capture
	// goroutine will ever close it. Stop closes the PipeWire capture to
	// unblock a captureLoop that is waiting on a frame, so done always
	// closes.
	mu      sync.Mutex
	started bool
	stopped bool

	// framesWritten counts H264 samples pushed to the track, for periodic
	// stats logging. Read/written atomically so it is safe from the capture
	// goroutine.
	framesWritten uint64
}

// newPipewireStreamer builds the portal + PipeWire pipeline and the H264
// track, but does not start pushing frames yet. Call Start to begin
// streaming.
//
// A non-nil error means the system cannot capture (no xdg-desktop-portal,
// the user cancelled the share dialog, no PipeWire, or the chosen encoder
// is unavailable); the caller should continue without video.
func newPipewireStreamer(cfg Config) (*pipewireStreamer, error) {
	if cfg.FrameRate <= 0 {
		cfg.FrameRate = 30
	}
	if cfg.QP <= 0 {
		cfg.QP = 24
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "desktop-remote",
	)
	if err != nil {
		return nil, fmt.Errorf("video: create track: %w", err)
	}

	enc, err := newEncoder(cfg, sourcePipewire)
	if err != nil {
		return nil, err
	}

	s := &pipewireStreamer{
		cfg:   cfg,
		track: track,
		enc:   enc,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}

	log.Printf("video: opening pipewire capture pipeline (%dfps, qp %d)", cfg.FrameRate, cfg.QP)
	log.Printf("video: pipewire: requesting screen share permission from the desktop...")

	// The portal flow shows the desktop's share dialog and blocks until the
	// user answers. It must succeed up front, so New fails fast when the
	// portal is missing or the user declines.
	portal, err := openScreenCastPortal()
	if err != nil {
		s.enc.free()
		return nil, err
	}
	s.portal = portal

	capture, err := pwOpen(portal.RemoteFD, portal.NodeID,
		portal.CaptureWidth, portal.CaptureHeight, cfg.FrameRate)
	if err != nil {
		portal.Close()
		s.enc.free()
		return nil, err
	}
	s.capture = capture

	w, h := capture.negotiatedSize()
	CaptureWidth.Store(int64(w))
	CaptureHeight.Store(int64(h))
	log.Printf("video: capture source ready (%dx%d via xdg-desktop-portal + pipewire)", w, h)
	return s, nil
}

// Start launches the capture/encode goroutine writing H264 samples to the
// track returned by Track. It returns immediately. The goroutine runs until
// Stop is called. Start is idempotent: only the first call launches the
// goroutine, and a call after Stop is a no-op (see mu/started/stopped).
func (s *pipewireStreamer) Start() {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	log.Printf("video: starting pipewire capture/encode goroutine")
	go s.captureLoop()
}

// Track returns the H264 WebRTC track the encoded samples are written to.
func (s *pipewireStreamer) Track() *webrtc.TrackLocalStaticSample { return s.track }

// Stop signals the capture goroutine to stop and frees all resources. It is
// safe to call multiple times, and safe to call before Start: a pipeline
// that was never started has no goroutine to wait for.
func (s *pipewireStreamer) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	started := s.started
	s.mu.Unlock()

	log.Printf("video: stopping pipewire capture pipeline")
	close(s.stop)
	// The capture loop may be blocked waiting for a frame (a static desktop
	// produces none); closing the capture unblocks it with
	// errPipewireClosed so done always closes.
	s.capture.close()
	if started {
		<-s.done
	}
	s.portal.Close()
	s.enc.free()
	CaptureWidth.Store(0)
	CaptureHeight.Store(0)
	log.Printf("video: pipewire capture pipeline stopped")
}

// captureLoop is the main capture/encode loop. It pulls frames from the
// PipeWire queue (which paces itself via damage events) and runs each
// through the shared encoder: filter graph (converting to the encoder's
// pixel format / uploading to its hardware device), encode, and write H264
// samples to the track. The frame-to-encode tail mirrors the x11grab loop;
// the difference is that frames arrive already decoded.
func (s *pipewireStreamer) captureLoop() {
	defer close(s.done)
	defer log.Printf("video: pipewire capture goroutine exited (%d frames written)", atomic.LoadUint64(&s.framesWritten))

	frameDuration := time.Duration(float64(time.Second) / float64(s.cfg.FrameRate))
	// Emit a stats line roughly every 10s.
	statsEvery := uint64(s.cfg.FrameRate * 10)
	if statsEvery == 0 {
		statsEvery = 300
	}

	// graphW/H/PixelFormat describe the frames the filter graph was built
	// for; a monitor mode change mid-stream renegotiates the PipeWire
	// format, which the fixed graph cannot handle, so capture stops.
	graphW, graphH := 0, 0
	var graphPixelFormat astiav.PixelFormat

	for {
		select {
		case <-s.stop:
			return
		default:
		}

		pwf, err := s.capture.read()
		if err != nil {
			if errors.Is(err, errPipewireClosed) {
				return
			}
			log.Printf("video: %v", err)
			return
		}

		if graphW != 0 && (pwf.width != graphW || pwf.height != graphH || pwf.pixelFormat != graphPixelFormat) {
			log.Printf("video: pipewire negotiated format changed mid-stream (%dx%d %s -> %dx%d %s); "+
				"restarting the stream is not supported, stopping capture",
				graphW, graphH, graphPixelFormat, pwf.width, pwf.height, pwf.pixelFormat)
			return
		}

		frame := astiav.AllocFrame()
		if frame == nil {
			log.Printf("video: alloc frame failed")
			return
		}
		frame.SetWidth(pwf.width)
		frame.SetHeight(pwf.height)
		frame.SetPixelFormat(pwf.pixelFormat)
		if err := frame.AllocBuffer(32); err != nil {
			log.Printf("video: alloc frame buffer: %v", err)
			frame.Free()
			return
		}
		if err := frame.Data().SetBytes(pwf.data, 1); err != nil {
			log.Printf("video: fill frame: %v", err)
			frame.Free()
			continue
		}

		if err := s.enc.initFilterGraph(frameMeta{
			Width:             pwf.width,
			Height:            pwf.height,
			PixelFormat:       pwf.pixelFormat,
			SampleAspectRatio: astiav.NewRational(1, 1),
			TimeBase:          astiav.NewRational(1, s.cfg.FrameRate),
		}); err != nil {
			log.Printf("video: init filter graph: %v", err)
			frame.Free()
			return
		}
		graphW, graphH, graphPixelFormat = pwf.width, pwf.height, pwf.pixelFormat

		if err := s.enc.buffersrcContext.AddFrame(frame, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
			log.Printf("video: add frame to filter: %v", err)
			frame.Free()
			continue
		}
		frame.Free()

		for {
			s.enc.filteredFrame.Unref()

			if err := s.enc.buffersinkContext.GetFrame(s.enc.filteredFrame, astiav.NewBuffersinkFlags()); err != nil {
				if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
					break
				}
				log.Printf("video: get filtered frame: %v", err)
				break
			}

			if err := s.enc.initEncoder(); err != nil {
				log.Printf("video: init encoder: %v", err)
				s.enc.filteredFrame.Unref()
				return
			}

			s.pts++
			s.enc.filteredFrame.SetPts(s.pts)

			if err := s.enc.encodeCodecContext.SendFrame(s.enc.filteredFrame); err != nil {
				log.Printf("video: send frame to encoder: %v", err)
				s.enc.filteredFrame.Unref()
				break
			}

			for {
				s.enc.encodePacket.Unref()

				if err := s.enc.encodeCodecContext.ReceivePacket(s.enc.encodePacket); err != nil {
					if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
						break
					}
					log.Printf("video: receive packet: %v", err)
					break
				}

				if err := s.track.WriteSample(media.Sample{
					Data:     s.enc.encodePacket.Data(),
					Duration: frameDuration,
				}); err != nil {
					log.Printf("video: write sample: %v", err)
				}

				n := atomic.AddUint64(&s.framesWritten, 1)
				if n == 1 {
					log.Printf("video: first H264 sample written (%d bytes)", len(s.enc.encodePacket.Data()))
				}
				if n%statsEvery == 0 {
					log.Printf("video: %d frames written", n)
				}
			}
		}
	}
}
