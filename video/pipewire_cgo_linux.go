//go:build linux

// Go wrapper over the libpipewire C bridge in pipewire_linux.c. Frames come
// out of the bridge as raw handles over dequeued PipeWire buffers
// (inara_pw_read_raw); copyRawInto performs the single per-frame memcpy —
// straight into an avbuffer belonging to an astiav.Frame — after which the
// handle is released, returning the PipeWire buffer to the stream. The
// capture itself is driven from the pipewireStreamer in
// video_pipewire_linux.go; the xdg-desktop-portal session that provides the
// remote fd and node id lives in portal_linux.go.

package video

/*
// The pipewire/spa headers live at fixed, arch-independent paths on all
// major distros. Using explicit flags instead of pkg-config avoids the Go
// tool rejecting libpipewire-0.3.pc's -fno-strict-overflow cflag
// (https://go.dev/s/invalidflag). libavutil comes via pkg-config (also used
// by filter_graph_device.go) so the AVFrame layout is the linked one.
#cgo linux CFLAGS: -I/usr/include/pipewire-0.3 -I/usr/include/spa-0.2
#cgo linux LDFLAGS: -lpipewire-0.3
#cgo pkg-config: libavutil
#include <stdlib.h>
#include <stdint.h>
#include <libavutil/frame.h>
#include "pipewire_linux.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/asticode/go-astiav"
)

// errPipewireClosed is returned by pwCapture.readRaw after the capture was
// stopped (via pwCapture.requestStop, called from pipewireStreamer.Stop).
var errPipewireClosed = errors.New("video: pipewire capture closed")

// pwCapture is a connected PipeWire capture stream producing raw RGB frames.
type pwCapture struct {
	h *C.inara_pw
}

// pwOpen connects to the PipeWire remote fd/node handed out by the
// xdg-desktop-portal ScreenCast session and blocks until the stream is
// streaming (format negotiated). width/height (0 if unknown) and framerate
// steer the negotiation defaults only.
func pwOpen(remoteFD int, nodeID uint32, width, height, framerate int) (*pwCapture, error) {
	var handle *C.inara_pw
	var cerr *C.char
	rc := C.inara_pw_open(C.int(remoteFD), C.uint32_t(nodeID), C.uint32_t(width),
		C.uint32_t(height), C.uint32_t(framerate), C.int(15), &handle, &cerr)
	if rc != 0 {
		msg := "unknown pipewire error"
		if cerr != nil {
			msg = C.GoString(cerr)
			C.free(unsafe.Pointer(cerr))
		}
		return nil, fmt.Errorf("video: pipewire capture: %s", msg)
	}
	return &pwCapture{h: handle}, nil
}

// negotiatedSize returns the negotiated capture resolution, valid after a
// successful pwOpen. (0, 0) means negotiation reported nothing, which cannot
// happen after a successful open.
func (c *pwCapture) negotiatedSize() (width, height int) {
	var w, h C.uint32_t
	var f C.enum_inara_pw_format
	C.inara_pw_negotiated_format(c.h, &w, &h, &f)
	return int(w), int(h)
}

// negotiatedFramerate returns the negotiated frame rate (e.g. 60 for 60/1).
// It returns 0 when the compositor gave no definite rate (common on
// damage-driven screencasts); the caller should then use its own default.
func (c *pwCapture) negotiatedFramerate() int {
	var n, d C.uint32_t
	C.inara_pw_negotiated_framerate(c.h, &n, &d)
	if n == 0 || d == 0 {
		return 0
	}
	// Round to the nearest whole fps (rates like 59.94 become 60); the
	// pace is governed by frame delivery either way.
	return int((uint32(n) + uint32(d)/2) / uint32(d))
}

// pwRawFrame is a dequeued PipeWire buffer available for reading. Copy it
// with copyRawInto, then release it (which re-queues the buffer).
type pwRawFrame struct {
	c  *pwCapture
	f  *C.inara_pw_raw_frame
	w  int
	h  int
	pf astiav.PixelFormat
}

// readRaw blocks until a frame is captured, the stream errors, or stopping
// was requested (then it returns errPipewireClosed).
func (c *pwCapture) readRaw() (*pwRawFrame, error) {
	var cf *C.inara_pw_raw_frame
	var cerr *C.char
	rc := C.inara_pw_read_raw(c.h, &cf, &cerr)
	switch rc {
	case 1:
		return nil, errPipewireClosed
	case 2:
		msg := "unknown pipewire stream error"
		if cerr != nil {
			msg = C.GoString(cerr)
			C.free(unsafe.Pointer(cerr))
		}
		return nil, fmt.Errorf("video: pipewire stream error: %s", msg)
	}

	var w, h C.uint32_t
	var stride C.int32_t
	var f C.enum_inara_pw_format
	C.inara_pw_raw_frame_info(cf, &w, &h, &f, &stride)
	pf, err := pwPixelFormat(f)
	if err != nil {
		C.inara_pw_raw_frame_release(c.h, cf)
		return nil, err
	}
	return &pwRawFrame{c: c, f: cf, w: int(w), h: int(h), pf: pf}, nil
}

// copyRawInto copies the captured pixels into frame's buffer. The frame
// must already have width/height/pixel format set and an allocated buffer
// (AllocBuffer). This is the single memcpy each frame undergoes between the
// PipeWire shared buffer and the filter graph.
func (r *pwRawFrame) copyRawInto(frame *astiav.Frame) error {
	af := (*C.AVFrame)(frame.UnsafePointer())
	if af == nil || af.data[0] == nil {
		return errors.New("video: pipewire copy into frame without buffer")
	}
	if rc := C.inara_pw_copy_frame(r.f, af.data[0], C.int32_t(af.linesize[0])); rc != 0 {
		return fmt.Errorf("video: pipewire copy frame failed (linesize %d too small for %dpx)",
			int(af.linesize[0]), r.w)
	}
	return nil
}

// release returns the frame's PipeWire buffer to the stream and frees the
// handle. It must be called exactly once per raw frame, before the capture
// is destroyed, and is idempotent.
func (r *pwRawFrame) release() {
	if r != nil && r.f != nil {
		C.inara_pw_raw_frame_release(r.c.h, r.f)
		r.f = nil
	}
}

// requestStop stops frame production and unblocks any pending readRaw with
// errPipewireClosed. The caller must then let the consumer drain (release
// its frames) before destroy.
func (c *pwCapture) requestStop() {
	if c.h != nil {
		C.inara_pw_request_stop(c.h)
	}
}

// destroy tears down the PipeWire stream after requestStop. All raw frames
// must have been released first.
func (c *pwCapture) destroy() {
	if c.h != nil {
		C.inara_pw_destroy(c.h)
		c.h = nil
	}
}

// pwPixelFormat maps the bridge format enum to the equivalent FFmpeg pixel
// format. All advertised formats are 32/24-bit RGB variants.
func pwPixelFormat(f C.enum_inara_pw_format) (astiav.PixelFormat, error) {
	switch f {
	case C.INARA_PW_FORMAT_BGRX:
		return astiav.PixelFormatBgr0, nil
	case C.INARA_PW_FORMAT_BGRA:
		return astiav.PixelFormatBgra, nil
	case C.INARA_PW_FORMAT_RGBX:
		return astiav.PixelFormatRgb0, nil
	case C.INARA_PW_FORMAT_RGBA:
		return astiav.PixelFormatRgba, nil
	case C.INARA_PW_FORMAT_RGB:
		return astiav.PixelFormatRgb24, nil
	case C.INARA_PW_FORMAT_BGR:
		return astiav.PixelFormatBgr24, nil
	default:
		return 0, fmt.Errorf("video: unsupported pipewire pixel format %d", int(f))
	}
}
