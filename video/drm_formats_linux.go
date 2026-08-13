//go:build linux

package video

/*
#cgo pkg-config: libdrm libavutil

#include <stdint.h>
#include <fcntl.h>
#include <unistd.h>
#include <xf86drm.h>
#include <xf86drmMode.h>
#include <libavutil/frame.h>
#include <libavutil/hwcontext.h>
#include <libavutil/hwcontext_drm.h>

// probePrimaryPlaneFormatC opens cardPath and returns the pixel format of the
// framebuffer on the first active primary plane (the desktop scanout buffer
// kmsgrab will capture). Returns 0 when it cannot be determined (no plane, no
// fb, GetFB2 unavailable, or open fails); the caller then makes no deep-color
// assumption and lets kmsgrab report its own error.
static uint32_t probePrimaryPlaneFormatC(const char *cardPath) {
	int fd = open(cardPath, O_RDWR | O_CLOEXEC);
	if (fd < 0)
		return 0;
	uint32_t result = 0;
	drmModePlaneRes *res = drmModeGetPlaneResources(fd);
	if (res) {
		for (uint32_t i = 0; i < res->count_planes && !result; i++) {
			drmModePlane *plane = drmModeGetPlane(fd, res->planes[i]);
			if (!plane)
				continue;
			if (plane->fb_id) {
				// drmModeGetFB2 is present in libdrm >= 2.4.101 (2020); the build
				// dependency is far newer, so call it unconditionally.
				drmModeFB2 *fb2 = drmModeGetFB2(fd, plane->fb_id);
				if (fb2) {
					result = fb2->pixel_format;
					drmModeFreeFB2(fb2);
				}
			}
			drmModeFreePlane(plane);
		}
		drmModeFreePlaneResources(res);
	}
	close(fd);
	return result;
}

// patchDRMFrameFormatC rewrites the DRM layer format(s) in the frame's DRM
// descriptor and the sw_format of the frame's hardware-frames context.
//
// kmsgrab produces AV_PIX_FMT_DRM_PRIME frames whose AVDRMFrameDescriptor is
// the frame's data[0] and whose hw_frames_ctx is a DRM frames context. When the
// real framebuffer fourcc is a deep-color format the demuxer does not know
// (e.g. a 10-bit/HDR desktop scanned out as ABGR16161616), we present it to
// FFmpeg as a format it does understand:
//
//   - desc->layers[i].format   so drm_map_frame builds the right plane layout
//     (offset/pitch are unchanged; the deep-color fourcc has the same 1-layer /
//     1-plane shape as the real one).
//   - frames_ctx->sw_format    so drm_map_from / hwdownload tag the mapped
//     software frame with the matching little-endian pixel format (this is what
//     actually tells downstream filters how to interpret the bytes).
//
// Returns the original layer fourcc, or 0 if the frame is not a DRM_PRIME frame.
static uint32_t patchDRMFrameFormatC(AVFrame *frame, uint32_t newFormat, int newSwFormat) {
	if (!frame || frame->format != AV_PIX_FMT_DRM_PRIME || !frame->data[0])
		return 0;
	AVDRMFrameDescriptor *desc = (AVDRMFrameDescriptor*)frame->data[0];
	uint32_t old = 0;
	if (desc->nb_layers > 0)
		old = desc->layers[0].format;
	for (int i = 0; i < desc->nb_layers; i++)
		desc->layers[i].format = newFormat;
	if (frame->hw_frames_ctx) {
		AVHWFramesContext *fctx = (AVHWFramesContext*)frame->hw_frames_ctx->data;
		fctx->sw_format = (enum AVPixelFormat)newSwFormat;
	}
	return old;
}
*/
import "C"

import (
	"unsafe"

	"github.com/asticode/go-astiav"
)

// DRM fourccs the in-tree kmsgrab demuxer does not know but that compositors
// use for 10-bit/HDR (16-bit-per-channel) scanout framebuffers. Compositors
// scan out 10-bit content on a 16:16:16:16 plane, so these are the formats hit
// in practice when a user enables 10-bit color / HDR. Each is mapped to the
// little-endian FFmpeg software pixel format that matches its memory layout.
// See drm_fourcc.h "[63:0] A:R:G:B 16:16:16:16 little endian".
const (
	drmFormatXRGB16161616 = 0x38345258 // fourcc 'X','R','4','8'
	drmFormatXBGR16161616 = 0x38344258 // fourcc 'X','B','4','8'
	drmFormatARGB16161616 = 0x38345241 // fourcc 'A','R','4','8'
	drmFormatABGR16161616 = 0x38344241 // fourcc 'A','B','4','8'
)

// deepColorToPixFmt maps a high bit-depth DRM fourcc to the FFmpeg software
// pixel format describing the same little-endian memory layout. ok is false
// for formats we cannot convert.
//
// The mapping is DRM (msb->lsb channel order in a native little-endian word)
// to byte order in memory. E.g. ABGR16161616 has A as the most significant
// 16-bit field and B as the least; in little-endian memory the bytes run
// B,G,R,A, which is exactly AV_PIX_FMT_RGBA64LE.
func deepColorToPixFmt(fourcc uint32) (astiav.PixelFormat, bool) {
	switch fourcc {
	case drmFormatXRGB16161616, drmFormatARGB16161616:
		return astiav.PixelFormatBgra64Le, true
	case drmFormatXBGR16161616, drmFormatABGR16161616:
		return astiav.PixelFormatRgba64Le, true
	}
	return 0, false
}

// patchDRMFrameFormat presents a DRM_PRIME frame whose real fourcc kmsgrab does
// not support as the deep-color format swFmt/newFourcc that FFmpeg understands.
// It rewrites the frame's DRM descriptor layer formats and the frame's
// hardware-frames-context sw_format, returning the previous fourcc (0 if the
// frame is not a DRM_PRIME frame). See the C comment for the rationale.
func patchDRMFrameFormat(frame *astiav.Frame, newFourcc uint32, swFmt astiav.PixelFormat) uint32 {
	if frame == nil {
		return 0
	}
	return uint32(C.patchDRMFrameFormatC(
		(*C.AVFrame)(frame.UnsafePointer()),
		C.uint32_t(newFourcc),
		C.int(swFmt),
	))
}

// probePrimaryPlaneFormat returns the DRM fourcc of the desktop scanout
// framebuffer kmsgrab will capture, or 0 when it cannot be determined.
func probePrimaryPlaneFormat(cardPath string) uint32 {
	c := C.CString(cardPath)
	defer C.free(unsafe.Pointer(c))
	return uint32(C.probePrimaryPlaneFormatC(c))
}
