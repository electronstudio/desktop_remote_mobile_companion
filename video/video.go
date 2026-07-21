//go:build linux

// Package video captures the Linux desktop via the kmsgrab DRM demuxer,
// scales/converts it with VAAPI, encodes it to H264 with the h264_vaapi
// hardware encoder, and writes the H264 samples to a Pion WebRTC track so
// they can be streamed to the mobile client.
//
// The pipeline mirrors the ffmpeg command:
//
//	ffmpeg -device /dev/dri/card0 -f kmsgrab -i - \
//	    -vf 'hwmap=derive_device=vaapi,scale_vaapi=format=nv12' \
//	    -c:v h264_vaapi -qp 24 -bf 0 -
//
// Only VAAPI-capable systems are supported. A software fallback (x11grab +
// libx264) is tracked in improvements.md.
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
)

// Config configures the capture pipeline.
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

// Streamer owns the capture + encode pipeline and the Pion H264 track that
// receives the encoded samples.
type Streamer struct {
	cfg   Config
	Track *webrtc.TrackLocalStaticSample

	stop chan struct{}
	done chan struct{}
	pts  int64

	// framesWritten counts H264 samples pushed to the track, for periodic
	// stats logging. Read/written atomically so it is safe from the capture
	// goroutine.
	framesWritten uint64 //nolint:unused // used for stats logging

	// encoderOpened / filterBuilt avoid re-logging one-time setup each frame.
	encoderOpened bool
	filterBuilt   bool

	// astiav objects, allocated lazily.
	inputFormatContext *astiav.FormatContext
	decodeCodecContext *astiav.CodecContext
	decodePacket       *astiav.Packet
	decodeFrame        *astiav.Frame
	videoStream        *astiav.Stream

	filterGraph       *astiav.FilterGraph
	buffersrcContext  *astiav.BuffersrcFilterContext
	buffersinkContext *astiav.BuffersinkFilterContext
	filteredFrame     *astiav.Frame

	encodeCodecContext *astiav.CodecContext
	encodePacket       *astiav.Packet
}

// New builds the kmsgrab + VAAPI pipeline and the H264 track, but does not
// start pushing frames yet. Call Start to begin streaming.
//
// A non-nil error means the system cannot capture (no VAAPI, no kmsgrab, no
// /dev/dri); the caller should continue without video.
func New(cfg Config) (*Streamer, error) {
	if cfg.FrameRate <= 0 {
		cfg.FrameRate = 30
	}
	if cfg.QP <= 0 {
		cfg.QP = 24
	}
	if cfg.LowPower != 0 && cfg.LowPower != 1 {
		cfg.LowPower = 1
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

	s := &Streamer{
		cfg:   cfg,
		Track: track,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}

	log.Printf("video: opening capture pipeline on %s (%dfps, qp %d, low-power %d)", cfg.CardPath, cfg.FrameRate, cfg.QP, cfg.LowPower)

	// Register FFmpeg devices (kmsgrab is an input device) and open the
	// capture source up front so New fails fast if the hardware is missing.
	astiav.RegisterAllDevices()

	if err := s.initKmsgrab(); err != nil {
		s.freeVideoCoding()
		return nil, err
	}

	log.Printf("video: capture source ready (%dx%d)", s.decodeCodecContext.Width(), s.decodeCodecContext.Height())
	return s, nil
}

// Start launches the capture/encode goroutine writing H264 samples to Track.
// It returns immediately. The goroutine runs until Stop is called.
func (s *Streamer) Start() {
	log.Printf("video: starting capture/encode goroutine")
	go s.captureLoop()
}

// Stop signals the capture goroutine to stop and frees all astiav resources.
// It is safe to call multiple times.
func (s *Streamer) Stop() {
	select {
	case <-s.stop:
		// already stopped
		return
	default:
		log.Printf("video: stopping capture pipeline")
		close(s.stop)
	}
	<-s.done
	s.freeVideoCoding()
	log.Printf("video: capture pipeline stopped")
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
			f.Close()
			return m, nil
		}
	}
	// None readable; return the first so the open error surfaces in initKmsgrab.
	return matches[0], nil
}

// initKmsgrab opens the kmsgrab input device and prepares the decoder.
func (s *Streamer) initKmsgrab() error {
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
	s.filteredFrame = astiav.AllocFrame()
	s.encodePacket = astiav.AllocPacket()
	return nil
}

// initFilterGraph lazily builds the hwmap + scale_vaapi filter graph the first
// time a decoded hardware frame is available.
func (s *Streamer) initFilterGraph() error {
	if s.filterGraph != nil {
		return nil
	}

	s.filterGraph = astiav.AllocFilterGraph()
	if s.filterGraph == nil {
		return errors.New("video: alloc filter graph")
	}

	outputs := astiav.AllocFilterInOut()
	defer outputs.Free()
	inputs := astiav.AllocFilterInOut()
	defer inputs.Free()

	buffersrc := astiav.FindFilterByName("buffer")
	if buffersrc == nil {
		return errors.New("video: buffer filter not found")
	}
	buffersink := astiav.FindFilterByName("buffersink")
	if buffersink == nil {
		return errors.New("video: buffersink filter not found")
	}

	var err error
	if s.buffersrcContext, err = s.filterGraph.NewBuffersrcFilterContext(buffersrc, "in"); err != nil {
		return fmt.Errorf("video: new buffersrc: %w", err)
	}
	if s.buffersinkContext, err = s.filterGraph.NewBuffersinkFilterContext(buffersink, "out"); err != nil {
		return fmt.Errorf("video: new buffersink: %w", err)
	}

	params := astiav.AllocBuffersrcFilterContextParameters()
	defer params.Free()
	params.SetHardwareFramesContext(s.decodeFrame.HardwareFramesContext())
	params.SetWidth(s.decodeCodecContext.Width())
	params.SetHeight(s.decodeCodecContext.Height())
	params.SetPixelFormat(s.decodeCodecContext.PixelFormat())
	params.SetSampleAspectRatio(s.decodeCodecContext.SampleAspectRatio())
	params.SetTimeBase(s.videoStream.TimeBase())

	if err := s.buffersrcContext.SetParameters(params); err != nil {
		return fmt.Errorf("video: set buffersrc params: %w", err)
	}
	if err := s.buffersrcContext.Initialize(nil); err != nil {
		return fmt.Errorf("video: init buffersrc: %w", err)
	}

	outputs.SetName("in")
	outputs.SetFilterContext(s.buffersrcContext.FilterContext())
	outputs.SetPadIdx(0)
	outputs.SetNext(nil)

	inputs.SetName("out")
	inputs.SetFilterContext(s.buffersinkContext.FilterContext())
	inputs.SetPadIdx(0)
	inputs.SetNext(nil)

	// hwmap derives its own VAAPI device from the kmsgrab DRM device, so (just
	// like the ffmpeg command line, which uses no -init_hw_device) we do not
	// attach a hardware device context to any filter here. Optionally cap
	// the width via scale_vaapi.
	filterDesc := "hwmap=derive_device=vaapi,scale_vaapi=format=nv12"
	if s.cfg.MaxWidth > 0 && s.decodeCodecContext.Width() > s.cfg.MaxWidth {
		filterDesc = fmt.Sprintf("hwmap=derive_device=vaapi,scale_vaapi=w=%d:format=nv12", s.cfg.MaxWidth)
	}

	if err := s.filterGraph.Parse(filterDesc, inputs, outputs); err != nil {
		return fmt.Errorf("video: parse filter graph: %w", err)
	}
	if err := s.filterGraph.Configure(); err != nil {
		return fmt.Errorf("video: configure filter graph: %w", err)
	}
	log.Printf("video: filter graph built (%s)", filterDesc)
	return nil
}

// initVideoEncoding lazily opens the h264_vaapi encoder once a filtered
// (NV12 VAAPI) frame is available.
func (s *Streamer) initVideoEncoding() error {
	if s.encodeCodecContext != nil {
		return nil
	}

	enc := astiav.FindEncoderByName("h264_vaapi")
	if enc == nil {
		return errors.New("video: h264_vaapi encoder not found")
	}

	s.encodeCodecContext = astiav.AllocCodecContext(enc)
	if s.encodeCodecContext == nil {
		return errors.New("video: alloc encode codec context")
	}

	s.encodeCodecContext.SetPixelFormat(s.filteredFrame.PixelFormat())
	s.encodeCodecContext.SetWidth(s.filteredFrame.Width())
	s.encodeCodecContext.SetHeight(s.filteredFrame.Height())
	s.encodeCodecContext.SetTimeBase(astiav.NewRational(1, s.cfg.FrameRate))
	s.encodeCodecContext.SetFramerate(astiav.NewRational(s.cfg.FrameRate, 1))

	// Borrow the VAAPI hardware frames context produced by scale_vaapi; the
	// encoder derives its VAAPI device from it.
	s.encodeCodecContext.SetHardwareFramesContext(s.filteredFrame.HardwareFramesContext())

	opts := astiav.NewDictionary()
	defer opts.Free()
	if err := opts.Set("qp", fmt.Sprintf("%d", s.cfg.QP), astiav.NewDictionaryFlags()); err != nil {
		return fmt.Errorf("video: set qp option: %w", err)
	}
	// No B-frames: WebRTC samples are written in encode order, and B-frames
	// would require reordering this simple sample writer does not do.
	if err := opts.Set("bf", "0", astiav.NewDictionaryFlags()); err != nil {
		return fmt.Errorf("video: set bf option: %w", err)
	}
	// low_power: 1 = use the fixed-function encode engine (faster, lower GPU
	// usage, slightly lower quality); 0 = use the shader-based encoder.
	if err := opts.Set("low_power", fmt.Sprintf("%d", s.cfg.LowPower), astiav.NewDictionaryFlags()); err != nil {
		return fmt.Errorf("video: set low_power option: %w", err)
	}

	if err := s.encodeCodecContext.Open(enc, opts); err != nil {
		return fmt.Errorf("video: open h264_vaapi encoder: %w", err)
	}
	log.Printf("video: h264_vaapi encoder opened (%dx%d, qp %d, low-power %d, timebase %s)",
		s.encodeCodecContext.Width(), s.encodeCodecContext.Height(),
		s.cfg.QP, s.cfg.LowPower, s.encodeCodecContext.TimeBase().String())
	return nil
}

// freeVideoCoding releases all astiav objects. Safe to call when partially
// initialized.
func (s *Streamer) freeVideoCoding() {
	if s.encodeCodecContext != nil {
		s.encodeCodecContext.Free()
		s.encodeCodecContext = nil
	}
	if s.encodePacket != nil {
		s.encodePacket.Free()
		s.encodePacket = nil
	}
	if s.filteredFrame != nil {
		s.filteredFrame.Free()
		s.filteredFrame = nil
	}
	if s.filterGraph != nil {
		s.filterGraph.Free()
		s.filterGraph = nil
	}
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

// captureLoop is the main capture/encode loop, ported from the reference
// example. kmsgrab paces ReadFrame at the configured frame rate, so no extra
// ticker is needed.
func (s *Streamer) captureLoop() {
	defer close(s.done)
	defer log.Printf("video: capture goroutine exited (%d frames written)", atomic.LoadUint64(&s.framesWritten))

	frameDuration := time.Duration(float64(time.Second) * float64(1) / float64(s.cfg.FrameRate))
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

			if err := s.initFilterGraph(); err != nil {
				log.Printf("video: init filter graph: %v", err)
				s.decodeFrame.Unref()
				return
			}

			if err := s.buffersrcContext.AddFrame(s.decodeFrame, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
				log.Printf("video: add frame to filter: %v", err)
				s.decodeFrame.Unref()
				continue
			}

			for {
				s.filteredFrame.Unref()

				if err := s.buffersinkContext.GetFrame(s.filteredFrame, astiav.NewBuffersinkFlags()); err != nil {
					if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
						break
					}
					log.Printf("video: get filtered frame: %v", err)
					break
				}

				if err := s.initVideoEncoding(); err != nil {
					log.Printf("video: init encoder: %v", err)
					s.filteredFrame.Unref()
					return
				}

				s.pts++
				s.filteredFrame.SetPts(s.pts)

				if err := s.encodeCodecContext.SendFrame(s.filteredFrame); err != nil {
					log.Printf("video: send frame to encoder: %v", err)
					s.filteredFrame.Unref()
					break
				}

				for {
					s.encodePacket.Unref()

					if err := s.encodeCodecContext.ReceivePacket(s.encodePacket); err != nil {
						if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
							break
						}
						log.Printf("video: receive packet: %v", err)
						break
					}

					// Write H264 (Annex B, as produced by h264_vaapi without a
					// global header) to the WebRTC track.
					if err := s.Track.WriteSample(media.Sample{
						Data:     s.encodePacket.Data(),
						Duration: frameDuration,
					}); err != nil {
						log.Printf("video: write sample: %v", err)
					}

					n := atomic.AddUint64(&s.framesWritten, 1)
					if n == 1 {
						log.Printf("video: first H264 sample written (%d bytes)", len(s.encodePacket.Data()))
					}
					if n%statsEvery == 0 {
						log.Printf("video: %d frames written", n)
					}
				}
			}
		}
	}
}
