// This file has no build tag: it is shared by all platforms because the
// encoder/filter logic is the same and the go-astiav dependency is available
// everywhere the video package builds. Platform-specific behaviour is
// isolated in the source streamers (video_linux.go / video_windows.go); this
// file is the orthogonal "encoder axis": given a capture source and a chosen
// H264 encoder family, it builds the matching filter graph (uploading to the
// encoder's hardware device when the source is software) and opens the
// encoder.

package video

import (
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/asticode/go-astiav"
)

// sourceKind identifies the capture backend. It determines whether the
// decoder produces hardware frames (which carry a hardware frames context
// the filter graph can derive a device from) or software frames (which must
// be uploaded to a hardware encoder via a device we create ourselves).
type sourceKind int

const (
	sourceKmsgrab sourceKind = iota
	sourceX11grab
	sourceDdagrab
)

func (k sourceKind) String() string {
	switch k {
	case sourceKmsgrab:
		return "kmsgrab"
	case sourceX11grab:
		return "x11grab"
	case sourceDdagrab:
		return "ddagrab"
	}
	return "unknown"
}

// hardwareInput reports whether the source's decoded frames carry a hardware
// frames context. kmsgrab produces DRM hardware frames and ddagrab produces
// D3D11 hardware frames; x11grab produces software frames.
func (k sourceKind) hardwareInput() bool {
	return k == sourceKmsgrab || k == sourceDdagrab
}

// isAMF/isMF classify per-vendor encoder strings into the families the shared
// encoder logic (hardware frames context, filter graph, option blocks) works
// with. The Windows build accepts "amf"/"h264_amf" and "mf"/"h264_mf"
// spellings for the explicit flag values; the HEVC variants are recognised
// for forward compatibility (WindowsEncoderLabels lists them).
func isAMF(label string) bool {
	switch label {
	case "amf", "h264_amf", "hevc_amf":
		return true
	}
	return false
}

func isMF(label string) bool {
	switch label {
	case "mf", "h264_mf", "hevc_mf":
		return true
	}
	return false
}

// encoderLabel is the resolved H264 encoder family.
type encoderLabel string

const (
	encVaapi   encoderLabel = "vaapi"
	encNvenc   encoderLabel = "nvenc"
	encAMF     encoderLabel = "amf"
	encMF      encoderLabel = "mf"
	encLibx264 encoderLabel = "libx264"
)

func (l encoderLabel) codecName() string {
	switch l {
	case encVaapi:
		return "h264_vaapi"
	case encNvenc:
		return "h264_nvenc"
	case encAMF:
		return "h264_amf"
	case encMF:
		return "h264_mf"
	case encLibx264:
		return "libx264"
	}
	return string(l)
}

// isHardwareLabel reports whether the encoder consumes hardware frames and
// therefore borrows the filter graph's hardware frames context.
func (l encoderLabel) isHardwareLabel() bool {
	return l == encVaapi || l == encNvenc || l == encAMF || l == encMF
}

// NvidiaGPU reports whether an NVIDIA device is present. It is a best-effort
// check for /dev/nvidia0, which exists on Linux systems with the NVIDIA
// driver loaded; on other platforms (or without the driver) it returns false.
// It is exported so main can warn about the kmsgrab-on-NVIDIA case.
func NvidiaGPU() bool {
	if _, err := os.Stat("/dev/nvidia0"); err == nil {
		return true
	}
	return false
}

func encoderAvailable(label encoderLabel) bool {
	return astiav.FindEncoderByName(label.codecName()) != nil
}

// resolveEncoder picks the encoder for the given source and config.
//
// Auto default on Linux: h264_nvenc on NVIDIA systems that have it (except
// kmsgrab, whose DRM frames cannot be fed to nvenc without a fragile
// vaapi->cuda transfer), else h264_vaapi if available, else libx264. With
// ddagrab (Windows) auto always resolves to libx264: no GPU vendor detection
// is done on Windows, so hardware encoding there is opt-in via
// --video-encoder={nvenc,amf,mf}; detecting the GPU vendor automatically is
// a documented possible future improvement. An explicit --video-encoder
// value is validated against the availability rules for its source.
func resolveEncoder(cfg Config, kind sourceKind) (encoderLabel, error) {
	requested := cfg.Encoder
	auto := requested == "" || requested == "auto"
	// The Windows-only encoders consume D3D11 hardware frames, so they can
	// only pair with ddagrab.
	windowsOnly := isAMF(requested) || isMF(requested)

	var label encoderLabel
	switch {
	case !auto:
		switch {
		case requested == "vaapi":
			label = encVaapi
		case requested == "nvenc":
			label = encNvenc
		case isAMF(requested):
			label = encAMF
		case isMF(requested):
			label = encMF
		case requested == "libx264":
			label = encLibx264
		default:
			if kind == sourceDdagrab {
				return "", fmt.Errorf("video: unsupported encoder %q for ddagrab (want nvenc, amf, mf, libx264, auto, or empty)", requested)
			}
			return "", fmt.Errorf("video: unsupported encoder %q (want vaapi, nvenc, libx264, auto, or empty)", requested)
		}
	case kind == sourceDdagrab:
		label = encLibx264
	case NvidiaGPU() && encoderAvailable(encNvenc) && kind != sourceKmsgrab:
		label = encNvenc
	case encoderAvailable(encVaapi):
		label = encVaapi
	default:
		label = encLibx264
	}

	// kmsgrab cannot feed nvenc; the Windows-only encoders require ddagrab.
	if kind == sourceKmsgrab && label == encNvenc {
		return "", errors.New("video: kmsgrab with nvenc is not supported; use --video-source x11grab")
	}
	if windowsOnly && kind != sourceDdagrab {
		return "", fmt.Errorf("video: encoder %q is only available with ddagrab on Windows", requested)
	}

	if !encoderAvailable(label) {
		return "", fmt.Errorf("video: encoder %s (%s) not available in this ffmpeg build", label, label.codecName())
	}
	return label, nil
}

// encoder owns the filter graph and H264 encoder shared across capture
// sources. A source streamer creates one via newEncoder and drives it from
// its capture loop: it feeds decoded frames into the filter graph and pulls
// encoded H264 packets out.
type encoder struct {
	cfg   Config
	kind  sourceKind
	label encoderLabel

	// hwDeviceContext is created up front for software sources that target a
	// hardware encoder (x11grab + vaapi/nvenc) so the hwupload filter can
	// derive frames. It is nil for hardware sources (kmsgrab/ddagrab — the
	// decoder's hardware frames context drives the filter graph) and for
	// libx264 (pure software).
	hwDeviceContext *astiav.HardwareDeviceContext

	filterGraph       *astiav.FilterGraph
	buffersrcContext  *astiav.BuffersrcFilterContext
	buffersinkContext *astiav.BuffersinkFilterContext
	filteredFrame     *astiav.Frame

	encodeCodecContext *astiav.CodecContext
	encodePacket       *astiav.Packet
}

// newEncoder resolves the encoder family, creates any hardware device context
// a software source needs for upload, and allocates the per-stream frame and
// packet. It does not build the filter graph or open the encoder yet; those
// are lazy (they need the first decoded/filtered frame, which carries the
// hardware frames context they borrow).
func newEncoder(cfg Config, kind sourceKind) (*encoder, error) {
	label, err := resolveEncoder(cfg, kind)
	if err != nil {
		return nil, err
	}

	e := &encoder{
		cfg:           cfg,
		kind:          kind,
		label:         label,
		filteredFrame: astiav.AllocFrame(),
		encodePacket:  astiav.AllocPacket(),
	}

	// Software sources targeting a hardware encoder need a hardware device
	// context for the hwupload filter to upload frames to.
	if !kind.hardwareInput() && (label == encVaapi || label == encNvenc) {
		var hwType astiav.HardwareDeviceType
		var device string
		if label == encVaapi {
			hwType = astiav.HardwareDeviceTypeVAAPI
			device = "" // default VAAPI render node
		} else {
			hwType = astiav.HardwareDeviceTypeCUDA
			device = "0" // first CUDA device
		}
		hdc, err := astiav.CreateHardwareDeviceContext(hwType, device, nil, 0)
		if err != nil {
			e.free()
			return nil, fmt.Errorf("video: create %s hardware device context: %w", label, err)
		}
		e.hwDeviceContext = hdc
		log.Printf("video: created %s hardware device context (device %q)", label, device)
	}

	log.Printf("video: encoder selected: %s (source=%s)", label, kind)
	if cfg.LowPower && label != encVaapi {
		log.Printf("video: low-power is a vaapi-only option; ignored for %s", label)
	}
	return e, nil
}

// initFilterGraph builds the filter graph the first time a decoded frame is
// available. The graph description depends on both the source (hardware vs
// software input frames) and the encoder (target device + required pixel
// format).
//
// For software sources uploading to a hardware encoder (x11grab + vaapi/nvenc)
// the graph contains an hwupload filter whose init callback requires the
// hardware device context to already be set. go-astiav's FilterGraph.Parse wraps
// avfilter_graph_parse_ptr, which creates AND initializes the parsed filters in
// one step, leaving no chance to set the device before hwupload's init (it fails
// with "A hardware device reference is required to upload frames to."). So that
// path uses parseFilterGraphWithDevice, which drives the segmented filtergraph
// API manually and attaches the device between create and init, exactly like
// FFmpeg's own fftools graph_parse(). Hardware sources (kmsgrab) and software
// encoders (libx264) have no hwupload and keep using FilterGraph.Parse.
func (e *encoder) initFilterGraph(decodeCodecContext *astiav.CodecContext, decodeFrame *astiav.Frame, videoStream *astiav.Stream) error {
	if e.filterGraph != nil {
		return nil
	}

	e.filterGraph = astiav.AllocFilterGraph()
	if e.filterGraph == nil {
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
	if e.buffersrcContext, err = e.filterGraph.NewBuffersrcFilterContext(buffersrc, "in"); err != nil {
		return fmt.Errorf("video: new buffersrc: %w", err)
	}
	if e.buffersinkContext, err = e.filterGraph.NewBuffersinkFilterContext(buffersink, "out"); err != nil {
		return fmt.Errorf("video: new buffersink: %w", err)
	}

	params := astiav.AllocBuffersrcFilterContextParameters()
	defer params.Free()
	if e.kind.hardwareInput() {
		params.SetHardwareFramesContext(decodeFrame.HardwareFramesContext())
	}
	params.SetWidth(decodeCodecContext.Width())
	params.SetHeight(decodeCodecContext.Height())
	params.SetPixelFormat(decodeCodecContext.PixelFormat())
	params.SetSampleAspectRatio(decodeCodecContext.SampleAspectRatio())
	params.SetTimeBase(videoStream.TimeBase())

	if err := e.buffersrcContext.SetParameters(params); err != nil {
		return fmt.Errorf("video: set buffersrc params: %w", err)
	}
	if err := e.buffersrcContext.Initialize(nil); err != nil {
		return fmt.Errorf("video: init buffersrc: %w", err)
	}

	outputs.SetName("in")
	outputs.SetFilterContext(e.buffersrcContext.FilterContext())
	outputs.SetPadIdx(0)
	outputs.SetNext(nil)

	inputs.SetName("out")
	inputs.SetFilterContext(e.buffersinkContext.FilterContext())
	inputs.SetPadIdx(0)
	inputs.SetNext(nil)

	filterDesc := e.filterGraphDesc(decodeCodecContext.Width())
	if e.hwDeviceContext != nil {
		// Software source -> hardware encoder: the graph contains hwupload,
		// which needs the device set before its init. Parse initializes too
		// early, so use the segmented-API helper.
		if err := parseFilterGraphWithDevice(e.filterGraph, filterDesc, inputs, outputs, e.hwDeviceContext); err != nil {
			return fmt.Errorf("video: parse filter graph %q: %w", filterDesc, err)
		}
	} else {
		if err := e.filterGraph.Parse(filterDesc, inputs, outputs); err != nil {
			return fmt.Errorf("video: parse filter graph %q: %w", filterDesc, err)
		}
	}

	if err := e.filterGraph.Configure(); err != nil {
		return fmt.Errorf("video: configure filter graph: %w", err)
	}
	log.Printf("video: filter graph built (%s)", filterDesc)
	return nil
}

// filterGraphDesc returns the filter chain for this (source, encoder) combo,
// applying the optional width cap. The kmsgrab+vaapi chain matches the
// original kmsgrab pipeline exactly; the others follow the equivalent ffmpeg
// command lines.
func (e *encoder) filterGraphDesc(srcWidth int) string {
	capped := e.cfg.MaxWidth > 0 && srcWidth > e.cfg.MaxWidth
	width := func() string {
		if capped {
			return fmt.Sprintf("w=%d:h=-2", e.cfg.MaxWidth)
		}
		return ""
	}

	switch {
	case e.kind == sourceKmsgrab && e.label == encVaapi:
		if capped {
			return fmt.Sprintf("hwmap=derive_device=vaapi,scale_vaapi=w=%d:format=nv12", e.cfg.MaxWidth)
		}
		return "hwmap=derive_device=vaapi,scale_vaapi=format=nv12"

	case e.kind == sourceKmsgrab && e.label == encLibx264:
		// Download the vaapi frame to the CPU for software encoding.
		base := "hwmap=derive_device=vaapi,scale_vaapi=format=nv12"
		if capped {
			base = fmt.Sprintf("hwmap=derive_device=vaapi,scale_vaapi=w=%d:format=nv12", e.cfg.MaxWidth)
		}
		return base + ",hwdownload,format=yuv420p"

	case e.kind == sourceX11grab && e.label == encLibx264:
		if capped {
			return fmt.Sprintf("scale=%s,format=yuv420p", width())
		}
		return "format=yuv420p"

	case e.kind == sourceX11grab && e.label == encVaapi:
		if capped {
			return fmt.Sprintf("scale=%s,format=nv12,hwupload=derive_device=vaapi", width())
		}
		return "format=nv12,hwupload=derive_device=vaapi"

	case e.kind == sourceX11grab && e.label == encNvenc:
		if capped {
			return fmt.Sprintf("scale=%s,format=nv12,hwupload=derive_device=cuda", width())
		}
		return "format=nv12,hwupload=derive_device=cuda"

	case e.kind == sourceDdagrab && e.label == encNvenc:
		// ddagrab frames live on the D3D11 device; upload them to CUDA for
		// nvenc via hwmap.
		if capped {
			return fmt.Sprintf("hwmap=derive_device=cuda,scale_cuda=w=%d:format=nv12", e.cfg.MaxWidth)
		}
		return "hwmap=derive_device=cuda,scale_cuda=format=nv12"

	case e.kind == sourceDdagrab && e.label == encAMF:
		// h264_amf consumes D3D11 frames natively; convert BGRA to NV12 on
		// the GPU via the D3D11 video processor (--video-width scaling is
		// Linux-only).
		return "scale_d3d11=format=nv12"

	case e.kind == sourceDdagrab && e.label == encMF:
		// h264_mf does NOT understand D3D11 hardware frames (it only accepts
		// software NV12/YUV420p; MF MFTs that take D3D11 surfaces always do
		// so inside their own device-context types) and scale_d3d11 fails on
		// many D3D11 devices (its RENDER_TARGET|VIDEO_ENCODER output texture
		// is rejected with E_INVALIDARG by WARP/feature-level-9 drivers and
		// some hybrid-GPU setups), so download to software BGRA (the frame's
		// native sw format — this device does not implement NV12 download as
		// a textured read, hence "Invalid output format nv12") and let the
		// MFT convert to NV12 and upload internally. This mirrors the widely
		// used
		//  ffmpeg -f lavfi -i "ddagrab,hwdownload,format=bgra" -c:v h264_mf
		// pipeline. An explicit software format=nv12 converts BGRA to NV12
		// (h264_mf requires NV12 or YUV420P input).
		return "hwdownload,format=bgra,format=nv12"

	case e.kind == sourceDdagrab && e.label == encLibx264:
		// Download the D3D11 frame to the CPU for software encoding. As for
		// h264_mf above, download in the native BGRA and convert in software.
		if capped {
			return fmt.Sprintf("hwdownload,format=bgra,scale=w=%d:h=-2:format=yuv420p", e.cfg.MaxWidth)
		}
		return "hwdownload,format=bgra,format=yuv420p"
	}

	return ""
}

// initEncoder opens the H264 encoder the first time a filtered frame is
// available, borrowing its hardware frames context (for hardware encoders)
// and pixel format. It is idempotent.
func (e *encoder) initEncoder() error {
	if e.encodeCodecContext != nil {
		return nil
	}

	enc := astiav.FindEncoderByName(e.label.codecName())
	if enc == nil {
		return fmt.Errorf("video: encoder %s not found", e.label.codecName())
	}

	e.encodeCodecContext = astiav.AllocCodecContext(enc)
	if e.encodeCodecContext == nil {
		return errors.New("video: alloc encode codec context")
	}

	e.encodeCodecContext.SetPixelFormat(e.filteredFrame.PixelFormat())
	e.encodeCodecContext.SetWidth(e.filteredFrame.Width())
	e.encodeCodecContext.SetHeight(e.filteredFrame.Height())
	e.encodeCodecContext.SetTimeBase(astiav.NewRational(1, e.cfg.FrameRate))
	e.encodeCodecContext.SetFramerate(astiav.NewRational(e.cfg.FrameRate, 1))

	// h264_mf: request low-latency mode (CODECAPI_AVLowLatencyMode) via the
	// generic codec flag; FF_DISABLE_AUTODETECT keeps mfenc from probing
	// unrelated output profiles slower.
	if e.label == encMF {
		e.encodeCodecContext.SetFlags(e.encodeCodecContext.Flags().Add(astiav.CodecContextFlagLowDelay))
	}

	// Keyframe interval. H264 P-frames are deltas against the previous
	// frame, so without periodic keyframes a single lost RTP packet freezes
	// the video forever (and a mid-stream joiner can never start decoding).
	// Set an explicit GOP of ~2s so the stream self-heals. This matters most
	// for nvenc, whose default GOP is effectively unbounded (one keyframe at
	// the very start, then P-frames forever); libvaapi already defaults its
	// GOP to the framerate, but set it explicitly for consistency.
	//
	// h264_mf is safe to include: mfenc.c maps any non-default gop_size
	// onto CODECAPI_AVEncMPVGOPSize, which Media Foundation H264 MFTs
	// support.
	e.encodeCodecContext.SetGopSize(e.cfg.FrameRate * 2)

	// Hardware encoders borrow the VAAPI/CUDA/D3D11 hardware frames context
	// produced by the filter graph; the encoder derives its device from it.
	// libx264 is pure software and needs no hardware context.
	if e.label.isHardwareLabel() {
		e.encodeCodecContext.SetHardwareFramesContext(e.filteredFrame.HardwareFramesContext())
	}

	opts := astiav.NewDictionary()
	defer opts.Free()
	switch e.label {
	case encVaapi:
		if err := opts.Set("qp", fmt.Sprintf("%d", e.cfg.QP), astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set qp option: %w", err)
		}
		// No B-frames: WebRTC samples are written in encode order.
		if err := opts.Set("bf", "0", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set bf option: %w", err)
		}
		// low_power: 1 = fixed-function encode engine (faster, lower GPU
		// usage, slightly lower quality); 0 = shader-based encoder.
		if e.cfg.LowPower {
			if err := opts.Set("low_power", "1", astiav.NewDictionaryFlags()); err != nil {
				return fmt.Errorf("video: set low_power option: %w", err)
			}
		}
	case encNvenc:
		// Constant-QP rate control, zero B-frames for WebRTC ordering.
		if err := opts.Set("rc", "constqp", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set rc option: %w", err)
		}
		if err := opts.Set("qp", fmt.Sprintf("%d", e.cfg.QP), astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set qp option: %w", err)
		}
		if err := opts.Set("bf", "0", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set bf option: %w", err)
		}
		// Low-latency tuning: ull tune + fastest preset + no encoder
		// frame-delay buffering. These named constants require the new
		// nvenc preset API (FFmpeg 7+).
		if err := opts.Set("tune", "ull", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set tune option: %w", err)
		}
		if err := opts.Set("preset", "p1", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set preset option: %w", err)
		}
		if err := opts.Set("delay", "0", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set delay option: %w", err)
		}
	case encAMF:
		// Ultra-low-latency usage, constant QP (lower means higher quality),
		// and no B-frames. repeat_headers inserts SPS/PPS into every
		// keyframe so the browser can start decoding mid-stream; the default
		// header_insertion_mode (gop) only inserts them on GOP boundaries,
		// which matches our ~2s GOP and keeps the payload small.
		if err := opts.Set("usage", "ultralowlatency", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set usage option: %w", err)
		}
		if err := opts.Set("rc", "cqp", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set rc option: %w", err)
		}
		if err := opts.Set("qp_i", fmt.Sprintf("%d", e.cfg.QP), astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set qp_i option: %w", err)
		}
		if err := opts.Set("qp_p", fmt.Sprintf("%d", e.cfg.QP), astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set qp_p option: %w", err)
		}
		if err := opts.Set("bf", "0", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set bf option: %w", err)
		}
		if err := opts.Set("repeat_headers", "1", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set repeat_headers option: %w", err)
		}
	case encMF:
		// FFmpeg's Media Foundation wrapper has no "low_latency"/repeat-
		// header options: low latency is requested via the generic
		// AV_CODEC_FLAG_LOW_DELAY codec flag (set before Open above) and SPS/
		// PPS are emitted by the MFT itself on keyframes (mfenc.c only puts
		// them in extradata for the MP4-style muxers).
		if err := opts.Set("rate_control", "u_vbr", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set rate_control option: %w", err)
		}
		if err := opts.Set("quality", fmt.Sprintf("%d", clampInt(100-e.cfg.QP*2, 0, 100)), astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set quality option: %w", err)
		}
		if err := opts.Set("hw_encoding", "1", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set hw_encoding option: %w", err)
		}
	case encLibx264:
		// Ultrafast + zerolatency minimises encode latency and CPU; crf is
		// quality (lower is better); baseline profile is WebRTC-friendly.
		if err := opts.Set("preset", "ultrafast", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set preset option: %w", err)
		}
		if err := opts.Set("tune", "zerolatency", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set tune option: %w", err)
		}
		if err := opts.Set("crf", fmt.Sprintf("%d", e.cfg.QP), astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set crf option: %w", err)
		}
		if err := opts.Set("bf", "0", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set bf option: %w", err)
		}
		if err := opts.Set("profile", "baseline", astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("video: set profile option: %w", err)
		}
	}

	if err := e.encodeCodecContext.Open(enc, opts); err != nil {
		return fmt.Errorf("video: open %s encoder: %w", e.label.codecName(), err)
	}
	log.Printf("video: %s encoder opened (%dx%d, timebase %s)",
		e.label.codecName(), e.encodeCodecContext.Width(), e.encodeCodecContext.Height(), e.encodeCodecContext.TimeBase().String())
	return nil
}

// clampInt clamps v to [lo, hi]. Used to map the QP-style quality setting
// onto h264_mf's 0..100 quality property (a higher quality property means
// higher quality, the opposite of QP).
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// free releases the filter graph, encoder, hardware device context and the
// per-stream frame/packet. It is safe to call on a partially-initialized
// encoder. The capture source owns and frees the input/decode objects.
func (e *encoder) free() {
	if e.encodeCodecContext != nil {
		e.encodeCodecContext.Free()
		e.encodeCodecContext = nil
	}
	if e.encodePacket != nil {
		e.encodePacket.Free()
		e.encodePacket = nil
	}
	if e.filteredFrame != nil {
		e.filteredFrame.Free()
		e.filteredFrame = nil
	}
	if e.filterGraph != nil {
		e.filterGraph.Free()
		e.filterGraph = nil
	}
	if e.hwDeviceContext != nil {
		e.hwDeviceContext.Free()
		e.hwDeviceContext = nil
	}
}
