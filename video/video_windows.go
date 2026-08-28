//go:build windows

// Package video captures the Windows desktop via the ddagrab (Direct3D 11
// Desktop Duplication API) source filter and feeds the D3D11 hardware frames
// to the shared H264 encoder (see encoder.go). This is the Windows
// counterpart to the Linux kmsgrab backend in video_linux.go: like kmsgrab,
// ddagrab produces hardware frames (sourceKind.hardwareInput), so the filter
// graph borrows the hardware frames context from the first captured frame
// rather than needing a device context created up front.
//
// ddagrab is an avfilter video source, consumed through the lavfi input
// format; the lavfi demuxer wraps each captured texture in a
// wrapped_avframe packet whose no-op decoder hands the D3D11 frame straight
// through. The whole thing mirrors the ffmpeg command:
//
//	ffmpeg -f lavfi -i ddagrab=framerate=30:draw_mouse=true \
//	    -c:v h264_mf out.mp4
//
// Encoder pairing is resolved by encoder.go: nvenc (via
// hwmap=derive_device=cuda,scale_cuda=format=nv12), h264_amf and h264_mf
// (both take the D3D11 frames natively via scale_d3d11=format=nv12), or
// libx264 (via hwdownload,format=nv12). On Windows the auto default is
// h264_mf: the Media Foundation transform targets whatever encoder Windows
// picks for the primary adapter (typically Intel Quick Sync), so no GPU
// vendor detection is needed. Auto preferring nvenc/amf over mf is a
// possible future improvement.
//
// Capture scope: the primary display (ddagrab output_idx=0) only; multi
// monitor and display selection are not possible/future improvements. The
// desktop mouse cursor is drawn into the captured frames (ddagrab's
// draw_mouse default).
//
// If New returns an error (e.g. no D3D11 device, or the chosen encoder is
// unavailable in this ffmpeg build), the caller continues without a video
// track; trackpad/tablet keep working.
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

// ddagrabStreamer owns the ddagrab input (via lavfi), the wrapped_avframe
// decode pipeline, and the shared encoder. It implements the video.Streamer
// interface.
type ddagrabStreamer struct {
	cfg   Config
	track *webrtc.TrackLocalStaticSample
	enc   *encoder

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
	// goroutine will ever close it.
	mu      sync.Mutex
	started bool
	stopped bool

	// framesWritten counts H264 samples pushed to the track, for periodic
	// stats logging. Read/written atomically so it is safe from the capture
	// goroutine.
	framesWritten uint64

	// astiav input/decode objects, allocated eagerly in newDdagrabStreamer.
	inputFormatContext *astiav.FormatContext
	decodeCodecContext *astiav.CodecContext
	decodePacket       *astiav.Packet
	decodeFrame        *astiav.Frame
	videoStream        *astiav.Stream
}

// newStreamer is the platform-specific dispatch called by the cross-platform
// New in video.go. On Windows the source is always ddagrab (the only Windows
// capture backend); --video-source is ignored.
func newStreamer(cfg Config) (Streamer, error) {
	return newDdagrabStreamer(cfg)
}

// newDdagrabStreamer builds the ddagrab pipeline and the H264 track, but does
// not start pushing frames yet. Call Start to begin streaming.
//
// A non-nil error means the system cannot capture (no D3D11 device, no
// ddagrab in this ffmpeg build, or the chosen encoder is unavailable); the
// caller should continue without video.
func newDdagrabStreamer(cfg Config) (*ddagrabStreamer, error) {
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

	enc, err := newEncoder(cfg, sourceDdagrab)
	if err != nil {
		return nil, err
	}

	s := &ddagrabStreamer{
		cfg:   cfg,
		track: track,
		enc:   enc,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}

	log.Printf("video: opening ddagrab capture pipeline (%dfps, qp %d)", cfg.FrameRate, cfg.QP)

	// Register FFmpeg devices (lavfi is an input device) and open the
	// capture source up front so New fails fast if D3D11 desktop
	// duplication is unavailable (e.g. no GPU/display, or a session where
	// the Desktop Duplication API cannot run).
	astiav.RegisterAllDevices()

	if err := s.initDdagrab(); err != nil {
		s.freeInputDecode()
		s.enc.free()
		return nil, err
	}

	CaptureWidth.Store(int64(s.decodeCodecContext.Width()))
	CaptureHeight.Store(int64(s.decodeCodecContext.Height()))
	log.Printf("video: capture source ready (%dx%d)", s.decodeCodecContext.Width(), s.decodeCodecContext.Height())
	return s, nil
}

// Start launches the capture/encode goroutine writing H264 samples to the
// track returned by Track. It returns immediately. The goroutine runs until
// Stop is called. Start is idempotent: only the first call launches the
// goroutine, and a call after Stop is a no-op (see mu/started/stopped).
func (s *ddagrabStreamer) Start() {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	log.Printf("video: starting ddagrab capture/encode goroutine")
	go s.captureLoop()
}

// Track returns the H264 WebRTC track the encoded samples are written to.
func (s *ddagrabStreamer) Track() *webrtc.TrackLocalStaticSample { return s.track }

// Stop signals the capture goroutine to stop and frees all resources. It is
// safe to call multiple times, and safe to call before Start: a pipeline
// that was never started has no goroutine to wait for.
func (s *ddagrabStreamer) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	started := s.started
	s.mu.Unlock()

	log.Printf("video: stopping ddagrab capture pipeline")
	close(s.stop)
	if started {
		<-s.done
	}
	s.freeInputDecode()
	s.enc.free()
	CaptureWidth.Store(0)
	CaptureHeight.Store(0)
	log.Printf("video: ddagrab capture pipeline stopped")
}

// initDdagrab opens the ddagrab filter through the lavfi input format and
// prepares the wrapped_avframe decoder. ddagrab captures the primary display
// (its default output_idx=0), draws the hardware mouse cursor into the
// frames (draw_mouse default), and paces production at cfg.FrameRate.
func (s *ddagrabStreamer) initDdagrab() error {
	s.inputFormatContext = astiav.AllocFormatContext()
	if s.inputFormatContext == nil {
		return errors.New("video: alloc input format context")
	}

	inputFormat := astiav.FindInputFormat("lavfi")
	if inputFormat == nil {
		return errors.New("video: lavfi input format not found (ffmpeg built without avfilter?)")
	}

	graph := fmt.Sprintf("ddagrab=framerate=%d", s.cfg.FrameRate)

	if err := s.inputFormatContext.OpenInput(graph, inputFormat, nil); err != nil {
		return fmt.Errorf("video: open lavfi %q: %w", graph, err)
	}

	if err := s.inputFormatContext.FindStreamInfo(nil); err != nil {
		return fmt.Errorf("video: find stream info: %w", err)
	}

	for _, st := range s.inputFormatContext.Streams() {
		if st.CodecParameters().MediaType() == astiav.MediaTypeVideo {
			s.videoStream = st
			break
		}
	}
	if s.videoStream == nil {
		return errors.New("video: no video stream in ddagrab output")
	}

	// lavfi wraps the source filter's frames in wrapped_avframe packets; the
	// no-op decoder unwraps them to D3D11 hardware frames.
	decodeCodec := astiav.FindDecoder(s.videoStream.CodecParameters().CodecID())
	if decodeCodec == nil {
		return errors.New("video: FindDecoder returned nil for wrapped_avframe codec")
	}

	s.decodeCodecContext = astiav.AllocCodecContext(decodeCodec)
	if s.decodeCodecContext == nil {
		return errors.New("video: alloc decode codec context")
	}

	if err := s.videoStream.CodecParameters().ToCodecContext(s.decodeCodecContext); err != nil {
		return fmt.Errorf("video: copy codec parameters: %w", err)
	}

	s.decodeCodecContext.SetFramerate(astiav.NewRational(s.cfg.FrameRate, 1))

	log.Printf("video: decoded stream %dx%d codec %s timebase %s",
		s.decodeCodecContext.Width(), s.decodeCodecContext.Height(),
		decodeCodec.Name(), s.videoStream.TimeBase().String())

	if err := s.decodeCodecContext.Open(decodeCodec, nil); err != nil {
		return fmt.Errorf("video: open decode codec: %w", err)
	}

	s.decodePacket = astiav.AllocPacket()
	s.decodeFrame = astiav.AllocFrame()
	return nil
}

// freeInputDecode releases the input and decode objects. Safe to call when
// partially initialized. The filter/encode objects are owned by s.enc.
func (s *ddagrabStreamer) freeInputDecode() {
	if s.decodeCodecContext != nil {
		s.decodeCodecContext.Free()
		s.decodeCodecContext = nil
	}
	if s.decodePacket != nil {
		s.decodePacket.Free()
		s.decodePacket = nil
	}
	if s.decodeFrame != nil {
		s.decodeFrame.Free()
		s.decodeFrame = nil
	}
	if s.inputFormatContext != nil {
		s.inputFormatContext.CloseInput()
		s.inputFormatContext.Free()
		s.inputFormatContext = nil
	}
}

// captureLoop is the main capture/encode loop. It mirrors the kmsgrab loop:
// ddagrab paces ReadFrame at the configured frame rate, each packet decodes
// 1:1 to a D3D11 frame through the wrapped_avframe decoder, and the frame
// runs through the encoder's filter graph (hwmap/hwdownload as the chosen
// encoder requires) into H264 samples on the track.
func (s *ddagrabStreamer) captureLoop() {
	defer close(s.done)
	defer log.Printf("video: ddagrab capture goroutine exited (%d frames written)", atomic.LoadUint64(&s.framesWritten))

	frameDuration := time.Duration(float64(time.Second) / float64(s.cfg.FrameRate))
	// Emit a stats line roughly every 10s.
	statsEvery := uint64(s.cfg.FrameRate * 10)
	if statsEvery == 0 {
		statsEvery = 300
	}

	for {
		select {
		case <-s.stop:
			return
		default:
		}

		s.decodePacket.Unref()

		if err := s.inputFormatContext.ReadFrame(s.decodePacket); err != nil {
			if errors.Is(err, astiav.ErrEof) {
				log.Printf("video: capture end of stream")
				return
			}
			// Transient read errors are logged and skipped rather than fatal.
			log.Printf("video: read frame error: %v", err)
			continue
		}

		s.decodePacket.RescaleTs(s.videoStream.TimeBase(), s.decodeCodecContext.TimeBase())

		if err := s.decodeCodecContext.SendPacket(s.decodePacket); err != nil {
			log.Printf("video: send packet error: %v", err)
			continue
		}

		for {
			if err := s.decodeCodecContext.ReceiveFrame(s.decodeFrame); err != nil {
				if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
					break
				}
				log.Printf("video: receive frame error: %v", err)
				break
			}

			if err := s.enc.initFilterGraph(frameMetaFromDecode(s.decodeCodecContext, s.decodeFrame, s.videoStream)); err != nil {
				log.Printf("video: init filter graph: %v", err)
				s.decodeFrame.Unref()
				return
			}

			if err := s.enc.buffersrcContext.AddFrame(s.decodeFrame, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
				log.Printf("video: add frame to filter: %v", err)
				s.decodeFrame.Unref()
				continue
			}

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

					// Write H264 to the WebRTC track. The web client needs
					// in-band parameter sets (Annex B): the Windows hardware
					// encoders are configured with repeat-headers options
					// (repeat_headers / header_insertion_mode) so keyframes
					// carry SPS/PPS, matching what h264_vaapi/nvenc/libx264
					// produce on Linux.
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
}
