//go:build linux

// Package video captures the Linux desktop via the x11grab input device and
// feeds the decoded (software) frames to the shared H264 encoder (see
// encoder.go). This is the software-input counterpart to the kmsgrab backend
// in video_linux.go: x11grab reads pixels from the X server instead of the DRM
// framebuffer, so it needs no CAP_SYS_ADMIN and works without VAAPI, but on a
// Wayland session it can only capture XWayland content (native Wayland
// surfaces are invisible to it) — main warns about that case.
//
// x11grab produces software frames, so it can pair with any encoder:
// libx264 (pure software), h264_vaapi (uploaded via hwupload), or h264_nvenc
// (uploaded to CUDA via hwupload). The encoder axis is handled by encoder.go;
// this file only owns the x11grab input and the (software) decode pipeline.
//
// If New returns an error (no x11grab, no X server, or the chosen encoder is
// unavailable), the caller should simply continue without a video track;
// trackpad/tablet keep working.
package video

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// x11grabStreamer owns the x11grab input, the software decode pipeline, and
// the shared encoder. It implements the video.Streamer interface.
type x11grabStreamer struct {
	cfg   Config
	track *webrtc.TrackLocalStaticSample
	enc   *encoder

	stop chan struct{}
	done chan struct{}
	pts  int64

	// framesWritten counts H264 samples pushed to the track, for periodic
	// stats logging. Read/written atomically so it is safe from the capture
	// goroutine.
	framesWritten uint64

	// astiav input/decode objects, allocated eagerly in newX11grabStreamer.
	inputFormatContext *astiav.FormatContext
	decodeCodecContext *astiav.CodecContext
	decodePacket       *astiav.Packet
	decodeFrame        *astiav.Frame
	videoStream        *astiav.Stream
}

// newX11grabStreamer builds the x11grab pipeline and the H264 track, but does
// not start pushing frames yet. Call Start to begin streaming.
//
// A non-nil error means the system cannot capture (no x11grab, no X server,
// or the chosen encoder is unavailable); the caller should continue without
// video.
func newX11grabStreamer(cfg Config) (*x11grabStreamer, error) {
	if cfg.FrameRate <= 0 {
		cfg.FrameRate = 30
	}
	if cfg.QP <= 0 {
		cfg.QP = 24
	}
	if cfg.LowPower != 0 && cfg.LowPower != 1 {
		cfg.LowPower = 1
	}

	display := os.Getenv("DISPLAY")
	if display == "" {
		display = ":0.0"
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "desktop-remote",
	)
	if err != nil {
		return nil, fmt.Errorf("video: create track: %w", err)
	}

	enc, err := newEncoder(cfg, sourceX11grab)
	if err != nil {
		return nil, err
	}

	s := &x11grabStreamer{
		cfg:   cfg,
		track: track,
		enc:   enc,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}

	log.Printf("video: opening x11grab capture pipeline on %s (%dfps, qp %d)", display, cfg.FrameRate, cfg.QP)

	// Register FFmpeg devices (x11grab is an input device) and open the
	// capture source up front so New fails fast if X is unavailable.
	astiav.RegisterAllDevices()

	if err := s.initX11grab(display); err != nil {
		s.freeInputDecode()
		s.enc.free()
		return nil, err
	}

	log.Printf("video: capture source ready (%dx%d, pixfmt %s)", s.decodeCodecContext.Width(), s.decodeCodecContext.Height(), s.decodeCodecContext.PixelFormat().String())
	return s, nil
}

// Start launches the capture/encode goroutine writing H264 samples to the
// track returned by Track. It returns immediately. The goroutine runs until
// Stop is called.
func (s *x11grabStreamer) Start() {
	log.Printf("video: starting x11grab capture/encode goroutine")
	go s.captureLoop()
}

// Track returns the H264 WebRTC track the encoded samples are written to.
func (s *x11grabStreamer) Track() *webrtc.TrackLocalStaticSample { return s.track }

// Stop signals the capture goroutine to stop and frees all resources. It is
// safe to call multiple times.
func (s *x11grabStreamer) Stop() {
	select {
	case <-s.stop:
		// already stopped
		return
	default:
		log.Printf("video: stopping x11grab capture pipeline")
		close(s.stop)
	}
	<-s.done
	s.freeInputDecode()
	s.enc.free()
	log.Printf("video: x11grab capture pipeline stopped")
}

// initX11grab opens the x11grab input device and prepares the (software)
// decoder. video_size is intentionally left unset so x11grab captures the
// whole screen (its default).
func (s *x11grabStreamer) initX11grab(display string) error {
	s.inputFormatContext = astiav.AllocFormatContext()
	if s.inputFormatContext == nil {
		return errors.New("video: alloc input format context")
	}

	inputFormat := astiav.FindInputFormat("x11grab")
	if inputFormat == nil {
		return errors.New("video: x11grab input format not found (ffmpeg built without x11grab?)")
	}

	deviceDictionary := astiav.NewDictionary()
	defer deviceDictionary.Free()
	if err := deviceDictionary.Set("framerate", fmt.Sprintf("%d", s.cfg.FrameRate), astiav.NewDictionaryFlags()); err != nil {
		return fmt.Errorf("video: set x11grab framerate option: %w", err)
	}
	// video_size is left unset: x11grab defaults to the whole screen.

	if err := s.inputFormatContext.OpenInput(display, inputFormat, deviceDictionary); err != nil {
		return fmt.Errorf("video: open x11grab %s: %w", display, err)
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
		return errors.New("video: no video stream in x11grab output")
	}

	decodeCodec := astiav.FindDecoder(s.videoStream.CodecParameters().CodecID())
	if decodeCodec == nil {
		return errors.New("video: FindDecoder returned nil for x11grab codec")
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
func (s *x11grabStreamer) freeInputDecode() {
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
// read a packet, decode it to a software frame, run it through the encoder's
// filter graph (which uploads to a hardware encoder when needed), encode, and
// write H264 samples to the track.
func (s *x11grabStreamer) captureLoop() {
	defer close(s.done)
	defer log.Printf("video: x11grab capture goroutine exited (%d frames written)", atomic.LoadUint64(&s.framesWritten))

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

			if err := s.enc.initFilterGraph(s.decodeCodecContext, s.decodeFrame, s.videoStream); err != nil {
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
