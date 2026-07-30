//go:build linux

// Package video captures the Linux desktop via the kmsgrab DRM demuxer and
// feeds the decoded frames to a shared H264 encoder (see encoder.go). The
// encoder builds the filter graph and opens the H264 encoder appropriate for
// the chosen --video-encoder (h264_vaapi, h264_nvenc, or libx264); this file
// only owns the kmsgrab input and the decode pipeline.
//
// This file is the Linux kmsgrab backend. The cross-platform Streamer
// interface, Config, and New constructor live in video.go; the shared
// encoder/filter logic lives in encoder.go; the x11grab backend lives in
// video_x11grab_linux.go.
//
// The kmsgrab + h264_vaapi pipeline mirrors the ffmpeg command:
//
//	ffmpeg -device /dev/dri/card0 -f kmsgrab -i - \
//	    -vf 'hwmap=derive_device=vaapi,scale_vaapi=format=nv12' \
//	    -c:v h264_vaapi -qp 24 -bf 0 -
//
// kmsgrab produces DRM hardware frames, so it pairs naturally with h264_vaapi
// (and, via hwdownload, libx264). It cannot feed h264_nvenc; resolveEncoder
// rejects that combination. On NVIDIA systems kmsgrab usually has no VAAPI,
// so main warns and the user should switch to --video-source x11grab.
//
// If New returns an error (no VAAPI, no kmsgrab, no /dev/dri), the caller
// should simply continue without a video track; trackpad/tablet keep working.
package video

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// kmsgrabStreamer owns the kmsgrab input, the decode pipeline, and the shared
// encoder. It implements the video.Streamer interface.
type kmsgrabStreamer struct {
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

	// astiav input/decode objects, allocated eagerly in newKmsgrabStreamer.
	inputFormatContext *astiav.FormatContext
	decodeCodecContext *astiav.CodecContext
	decodePacket       *astiav.Packet
	decodeFrame        *astiav.Frame
	videoStream        *astiav.Stream
}

// newStreamer is the platform-specific dispatch called by the cross-platform
// New in video.go. On Linux it routes the requested source to the matching
// backend.
func newStreamer(cfg Config) (Streamer, error) {
	switch cfg.Source {
	case "", "kmsgrab":
		return newKmsgrabStreamer(cfg)
	case "x11grab":
		return newX11grabStreamer(cfg)
	default:
		return nil, fmt.Errorf("video: unsupported source %q on linux (want kmsgrab or x11grab)", cfg.Source)
	}
}

// newKmsgrabStreamer builds the kmsgrab pipeline and the H264 track, but does
// not start pushing frames yet. Call Start to begin streaming.
//
// A non-nil error means the system cannot capture (no VAAPI, no kmsgrab, no
// /dev/dri, or the chosen encoder is unavailable); the caller should continue
// without video.
func newKmsgrabStreamer(cfg Config) (*kmsgrabStreamer, error) {
	if cfg.FrameRate <= 0 {
		cfg.FrameRate = 30
	}
	if cfg.QP <= 0 {
		cfg.QP = 24
	}
	if cfg.CardPath == "" {
		card, err := autoDetectCard()
		if err != nil {
			return nil, fmt.Errorf("video: auto-detect DRM card: %w", err)
		}
		cfg.CardPath = card
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "desktop-remote",
	)
	if err != nil {
		return nil, fmt.Errorf("video: create track: %w", err)
	}

	enc, err := newEncoder(cfg, sourceKmsgrab)
	if err != nil {
		return nil, err
	}

	s := &kmsgrabStreamer{
		cfg:   cfg,
		track: track,
		enc:   enc,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}

	log.Printf("video: opening kmsgrab capture pipeline on %s (%dfps, qp %d, low-power %t)", cfg.CardPath, cfg.FrameRate, cfg.QP, cfg.LowPower)

	// Register FFmpeg devices (kmsgrab is an input device) and open the
	// capture source up front so New fails fast if the hardware is missing.
	astiav.RegisterAllDevices()

	if err := s.initKmsgrab(); err != nil {
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
// Stop is called.
func (s *kmsgrabStreamer) Start() {
	log.Printf("video: starting kmsgrab capture/encode goroutine")
	go s.captureLoop()
}

// Track returns the H264 WebRTC track the encoded samples are written to.
func (s *kmsgrabStreamer) Track() *webrtc.TrackLocalStaticSample { return s.track }

// Stop signals the capture goroutine to stop and frees all resources. It is
// safe to call multiple times.
func (s *kmsgrabStreamer) Stop() {
	select {
	case <-s.stop:
		// already stopped
		return
	default:
		log.Printf("video: stopping kmsgrab capture pipeline")
		close(s.stop)
	}
	<-s.done
	s.freeInputDecode()
	s.enc.free()
	CaptureWidth.Store(0)
	CaptureHeight.Store(0)
	log.Printf("video: kmsgrab capture pipeline stopped")
}

// autoDetectCard returns the first /dev/dri/card* device path (sorted) that
// the process can open, falling back to the first one found if none are
// readable (kmsgrab open will report the real error).
func autoDetectCard() (string, error) {
	matches, err := filepath.Glob("/dev/dri/card*")
	if err != nil {
		return "", err
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return "", errors.New("no /dev/dri/card* devices found")
	}
	for _, m := range matches {
		f, err := os.OpenFile(m, os.O_RDWR, 0)
		if err == nil {
			_ = f.Close()
			return m, nil
		}
	}
	// None readable; return the first so the open error surfaces in initKmsgrab.
	return matches[0], nil
}

// initKmsgrab opens the kmsgrab input device and prepares the decoder.
func (s *kmsgrabStreamer) initKmsgrab() error {
	// attempt to raise caps for situation where we dont have sudo
	// and we dont have input group access to
	// uinput but we do have libcap ability to get sys admin caps
	orig := cap.GetProc()
	//defer orig.SetProc() // dont think we can restore original caps on exit without breaking video?
	c, err := orig.Dup()
	if err == nil {
		if err := c.SetFlag(cap.Effective, true, cap.SYS_ADMIN); err == nil {
			_ = c.SetProc()
		}
	}

	s.inputFormatContext = astiav.AllocFormatContext()
	if s.inputFormatContext == nil {
		return errors.New("video: alloc input format context")
	}

	inputFormat := astiav.FindInputFormat("kmsgrab")
	if inputFormat == nil {
		return errors.New("video: kmsgrab input format not found (ffmpeg built without libdrm?)")
	}

	deviceDictionary := astiav.NewDictionary()
	defer deviceDictionary.Free()
	if err := deviceDictionary.Set("device", s.cfg.CardPath, astiav.NewDictionaryFlags()); err != nil {
		return fmt.Errorf("video: set kmsgrab device option: %w", err)
	}

	if err := s.inputFormatContext.OpenInput("-", inputFormat, deviceDictionary); err != nil {
		return fmt.Errorf("video: open kmsgrab %s: %w", s.cfg.CardPath, err)
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
		return errors.New("video: no video stream in kmsgrab output")
	}

	decodeCodec := astiav.FindDecoder(s.videoStream.CodecParameters().CodecID())
	if decodeCodec == nil {
		return errors.New("video: FindDecoder returned nil for kmsgrab codec")
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
func (s *kmsgrabStreamer) freeInputDecode() {
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

// captureLoop is the main capture/encode loop. kmsgrab paces ReadFrame at the
// configured frame rate, so no extra ticker is needed.
func (s *kmsgrabStreamer) captureLoop() {
	defer close(s.done)
	defer log.Printf("video: kmsgrab capture goroutine exited (%d frames written)", atomic.LoadUint64(&s.framesWritten))

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
			// Transient read errors (e.g. DRM page-flip timing) are logged and
			// skipped rather than fatal.
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

					// Write H264 (Annex B, as produced by h264_vaapi without a
					// global header) to the WebRTC track.
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
