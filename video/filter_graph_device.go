// This file has no build tag: it is shared by all platforms the video package
// builds on, exactly like encoder.go. It only uses libavfilter, which go-astiav
// already links everywhere it compiles. It is only invoked for the
// software-source + hardware-encoder path (x11grab + vaapi/nvenc), which is
// Linux-only today, so on other platforms it is dead code that still compiles.

package video

/*
#cgo pkg-config: libavfilter libavutil

#include <libavfilter/avfilter.h>
#include <libavutil/buffer.h>
#include <libavutil/error.h>

// parseFilterGraphWithDeviceC builds the filter graph described by desc into
// graph, mirroring FFmpeg's fftools graph_parse() (the segmented filtergraph
// API): parse -> create_filters -> attach hw_device -> apply_opts -> init ->
// link.
//
// The device is attached AFTER create_filters but BEFORE init because filters
// such as hwupload require avctx->hw_device_ctx during their init callback.
// go-astiav's FilterGraph.Parse wraps avfilter_graph_parse_ptr, which creates
// AND initializes the parsed filters in one shot (segment_apply runs
// segment_init internally), leaving no window to set hw_device_ctx on a parsed
// hwupload before its init -- so hwupload fails with "A hardware device
// reference is required to upload frames to." Driving the public segmented
// steps manually gives us that window, exactly like ffmpeg's own CLI.
//
// open_inputs / open_outputs are the caller's AVFilterInOut lists: open_outputs
// is the external [in] source (the buffersrc), open_inputs is the external [out]
// sink (the buffersink). This supports a linear graph with one external source
// and one external sink: the segment's first open input is linked to the
// caller's source and its first open output to the caller's sink. On success
// the consumed caller inouts are freed and the caller's pointers are cleared
// (set to NULL). On error the caller still owns its inouts and must free them;
// filters this function created are left in the graph and are released later
// by avfilter_graph_free (FilterGraph.Free).
//
// hw_device may be NULL, in which case the device-attach step is skipped (this
// then behaves like a normal segmented parse + init + link).
//
// Returns 0 on success or a negative AVERROR code.
static int parseFilterGraphWithDeviceC(
	AVFilterGraph *graph, const char *desc,
	AVFilterInOut **open_inputs, AVFilterInOut **open_outputs,
	AVBufferRef *hw_device)
{
	AVFilterGraphSegment *seg = NULL;
	AVFilterInOut *seg_inputs = NULL, *seg_outputs = NULL;
	AVFilterInOut *src = NULL, *sink = NULL;
	int ret;

	if ((ret = avfilter_graph_segment_parse(graph, desc, 0, &seg)) < 0)
		goto end;
	if ((ret = avfilter_graph_segment_create_filters(seg, 0)) < 0)
		goto end;

	// Attach the device to every filter that can use one, before init.
	if (hw_device) {
		for (int i = 0; i < (int)graph->nb_filters; i++) {
			AVFilterContext *f = graph->filters[i];
			if (!(f->filter->flags & AVFILTER_FLAG_HWDEVICE))
				continue;
			av_buffer_unref(&f->hw_device_ctx);
			f->hw_device_ctx = av_buffer_ref(hw_device);
			if (!f->hw_device_ctx) { ret = AVERROR(ENOMEM); goto end; }
		}
	}

	if ((ret = avfilter_graph_segment_apply_opts(seg, 0)) < 0)
		goto end;
	if ((ret = avfilter_graph_segment_init(seg, 0)) < 0)
		goto end;
	if ((ret = avfilter_graph_segment_link(seg, 0, &seg_inputs, &seg_outputs)) < 0)
		goto end;

	// Positional matching for a single-source / single-sink linear graph:
	// the segment's first open input is fed by the caller's source, and its
	// first open output feeds the caller's sink. Reject any graph shape that
	// is not a single external source feeding a single external sink.
	src  = (open_outputs && *open_outputs) ? *open_outputs : NULL;
	sink = (open_inputs  && *open_inputs)  ? *open_inputs  : NULL;
	if ((seg_inputs  && (!src  || seg_inputs->next)) ||
	    (seg_outputs && (!sink || seg_outputs->next))) {
		ret = AVERROR(EINVAL);
		goto end;
	}
	if (seg_inputs && src) {
		if ((ret = avfilter_link(src->filter_ctx, src->pad_idx,
		                         seg_inputs->filter_ctx, seg_inputs->pad_idx)) < 0)
			goto end;
	}
	if (seg_outputs && sink) {
		if ((ret = avfilter_link(seg_outputs->filter_ctx, seg_outputs->pad_idx,
		                         sink->filter_ctx, sink->pad_idx)) < 0)
			goto end;
	}

	// The caller's inouts were consumed: free them and clear the pointers so
	// the caller's deferred FilterInOut.Free is a no-op (matches parse_ptr).
	if (open_outputs) { avfilter_inout_free(open_outputs); *open_outputs = NULL; }
	if (open_inputs)  { avfilter_inout_free(open_inputs);  *open_inputs  = NULL; }
	ret = 0;

end:
	avfilter_inout_free(&seg_inputs);
	avfilter_inout_free(&seg_outputs);
	avfilter_graph_segment_free(&seg);
	return ret;
}
*/
import "C"

import (
	"unsafe"

	"github.com/asticode/go-astiav"
)

// parseFilterGraphWithDevice is FilterGraph.Parse plus the ability to attach a
// hardware device context to hwupload-style filters before they initialize.
//
// It is used only for software capture sources (x11grab) targeting a hardware
// encoder (vaapi/nvenc), whose filter graph contains an hwupload filter that
// requires the device at init time. Hardware sources (kmsgrab) and software
// encoders (libx264) have no hwupload and keep using FilterGraph.Parse.
//
// go-astiav does not expose the raw libavfilter C pointers of FilterGraph,
// FilterInOut or HardwareDeviceContext (only CodecContext and Frame expose
// UnsafePointer). Each of those structs stores its C pointer as the first
// field, so we read it here via unsafe. This is coupled to the pinned
// go-astiav v0.41.0 struct layout; re-verify on upgrade.
func parseFilterGraphWithDevice(g *astiav.FilterGraph, desc string, inputs, outputs *astiav.FilterInOut, hwdev *astiav.HardwareDeviceContext) error {
	graphC := *(**C.AVFilterGraph)(unsafe.Pointer(g))

	var hwC *C.AVBufferRef
	if hwdev != nil {
		hwC = *(**C.AVBufferRef)(unsafe.Pointer(hwdev))
	}

	// Pass the address of each FilterInOut's first field (its `c` pointer) so
	// the C helper can read, consume/free, and clear it.
	var inC, outC **C.AVFilterInOut
	if inputs != nil {
		inC = (**C.AVFilterInOut)(unsafe.Pointer(inputs))
	}
	if outputs != nil {
		outC = (**C.AVFilterInOut)(unsafe.Pointer(outputs))
	}

	cdesc := C.CString(desc)
	defer C.free(unsafe.Pointer(cdesc))
	if ret := C.parseFilterGraphWithDeviceC(graphC, cdesc, inC, outC, hwC); ret < 0 {
		return astiav.Error(int(ret))
	}
	return nil
}
