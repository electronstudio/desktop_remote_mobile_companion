//go:build linux

// Go wrapper over the libpipewire C bridge in pipewire_linux.c. It converts
// C results into Go values (malloc'd C frames are copied with C.GoBytes and
// immediately freed) and maps the bridge's format enum onto go-astiav pixel
// formats. The capture itself is driven from the pipewireStreamer in
// video_pipewire_linux.go; the xdg-desktop-portal session that provides the
// remote fd and node id lives in portal_linux.go.

package video

/*
// The pipewire/spa headers live at fixed, arch-independent paths on all
// major distros. Using explicit flags instead of pkg-config avoids the Go
// tool rejecting libpipewire-0.3.pc's -fno-strict-overflow cflag
// (https://go.dev/s/invalidflag).
#cgo linux CFLAGS: -I/usr/include/pipewire-0.3 -I/usr/include/spa-0.2
#cgo linux LDFLAGS: -lpipewire-0.3
#include <stdlib.h>
#include "pipewire_linux.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/asticode/go-astiav"
)

// errPipewireClosed is returned by pwCapture.read after the capture was
// closed (via pwCapture.close, called from pipewireStreamer.Stop).
var errPipewireClosed = errors.New("video: pipewire capture closed")

// pwCapture is a connected PipeWire capture stream producing tightly packed
// 32-bit RGB frames.
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

// pwFrame is one captured frame in tightly packed bytes (stride == width*4).
type pwFrame struct {
	data          []byte
	width, height int
	pixelFormat   astiav.PixelFormat
}

// read blocks until a frame is captured, the stream errors, or the capture
// is closed (then it returns errPipewireClosed).
func (c *pwCapture) read() (pwFrame, error) {
	var cf *C.struct_inara_pw_frame
	var cerr *C.char
	rc := C.inara_pw_read(c.h, &cf, &cerr)
	switch rc {
	case 1:
		return pwFrame{}, errPipewireClosed
	case 2:
		msg := "unknown pipewire stream error"
		if cerr != nil {
			msg = C.GoString(cerr)
			C.free(unsafe.Pointer(cerr))
		}
		return pwFrame{}, fmt.Errorf("video: pipewire stream error: %s", msg)
	}
	defer C.inara_pw_frame_free(cf)

	pf, err := pwPixelFormat(cf.format)
	if err != nil {
		return pwFrame{}, err
	}
	return pwFrame{
		data:        C.GoBytes(unsafe.Pointer(cf.data), C.int(cf.size)),
		width:       int(cf.width),
		height:      int(cf.height),
		pixelFormat: pf,
	}, nil
}

// close tears down the PipeWire stream and unblocks any pending read with
// errPipewireClosed.
func (c *pwCapture) close() {
	if c.h != nil {
		C.inara_pw_close(c.h)
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
