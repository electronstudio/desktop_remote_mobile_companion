// C bridge to libpipewire for the "pipewire" video source (xdg-desktop-portal
// + PipeWire screen capture). This file and pipewire_linux.c know nothing
// about Go: the Go side (pipewire_cgo_linux.go) calls the functions below
// and the C side never calls back into Go, so no Go function pointers cross
// the boundary.
//
// Frames are NOT copied on the PipeWire thread-loop thread. The process
// event stashes the dequeued PipeWire buffer into a bounded (drop-oldest)
// queue of inara_pw_raw_frame handles; the consumer copies the frame
// exactly once — directly into its own destination buffer — with
// inara_pw_copy_frame, and returns the PipeWire buffer to the stream with
// inara_pw_raw_frame_release. The bridge stays FFmpeg-free: consumers pass
// plain pointers (the Go side gets them from an AVFrame).
//
// Teardown is two-phase: inara_pw_request_stop stops frame production and
// unblocks a pending inara_pw_read_raw; once every outstanding raw frame has
// been released, inara_pw_destroy frees the stream.

#ifndef INARA_PIPEWIRE_H
#define INARA_PIPEWIRE_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// Opaque capture handle.
typedef struct inara_pw inara_pw;

// Opaque handle for one dequeued PipeWire buffer. Returned by
// inara_pw_read_raw, consumed by inara_pw_copy_frame, and given back with
// inara_pw_raw_frame_release (which re-queues the buffer to the stream).
typedef struct inara_pw_raw_frame inara_pw_raw_frame;

// inara_pw_format is the bridge's own pixel-format enum (so the Go side never
// has to hardcode SPA_VIDEO_FORMAT_* values). Only these RGB formats are
// ever advertised to PipeWire.
enum inara_pw_format {
	INARA_PW_FORMAT_UNKNOWN = 0,
	INARA_PW_FORMAT_BGRX,
	INARA_PW_FORMAT_BGRA,
	INARA_PW_FORMAT_RGBX,
	INARA_PW_FORMAT_RGBA,
	INARA_PW_FORMAT_RGB,
	INARA_PW_FORMAT_BGR
};

// inara_pw_open connects to the PipeWire remote on remote_fd (duplicated
// internally; the caller keeps ownership), creates a capture stream to
// node_id, negotiates a raw video format (one of the formats above) and
// blocks until the stream is STREAMING, which includes format negotiation.
// width/height (from the portal session, 0 if unknown) and framerate only
// steer the negotiation defaults; the actually negotiated values may differ
// — query them with inara_pw_negotiated_format/framerate.
//
// Returns 0 on success. On failure returns != 0 and sets *errmsg to a
// malloc'd message the caller must free().
int inara_pw_open(int remote_fd, uint32_t node_id, uint32_t width,
		  uint32_t height, uint32_t framerate, int timeout_seconds,
		  inara_pw **out, char **errmsg);

// inara_pw_negotiated_format reports the format PipeWire negotiated. It is
// valid once inara_pw_open returned 0.
void inara_pw_negotiated_format(const inara_pw *pw, uint32_t *width,
				uint32_t *height, enum inara_pw_format *format);

// inara_pw_negotiated_framerate reports the negotiated frame rate as a
// fraction (e.g. 60/1). num/denom are 0 when the negotiated rate is unset or
// unspecified (a 0 denominator means "unknown", common on damage-driven
// screencasts).
void inara_pw_negotiated_framerate(const inara_pw *pw, uint32_t *num,
				   uint32_t *denom);

// inara_pw_raw_frame_info describes the next frame. All out parameters are
// required. width/height in pixels, format the pixel format, stride the
// source row stride in bytes (may exceed width * bytes_per_pixel).
void inara_pw_raw_frame_info(const inara_pw_raw_frame *f, uint32_t *width,
			     uint32_t *height, enum inara_pw_format *format,
			     int32_t *stride);

// inara_pw_read_raw blocks until a frame is available, stopping was
// requested, or the stream reports an error.
//
// Returns 0 and sets *out on success; returns 1 when stopping was requested
// with inara_pw_request_stop; returns 2 on a stream error (errmsg is a
// malloc'd message the caller must free()).
int inara_pw_read_raw(inara_pw *pw, inara_pw_raw_frame **out, char **errmsg);

// inara_pw_copy_frame copies the frame rows into dst, honouring
// dst_linesize (bytes per destination row, >= width * bytes_per_pixel).
// Returns 0 on success, -1 if the frame is invalid.
int inara_pw_copy_frame(const inara_pw_raw_frame *f, uint8_t *dst,
			int32_t dst_linesize);

// inara_pw_raw_frame_release returns the frame's PipeWire buffer to the
// stream and frees the handle. Frames returned by inara_pw_read_raw are
// reference-counted against the stream: every one must be released exactly
// once, before inara_pw_destroy.
void inara_pw_raw_frame_release(inara_pw *pw, inara_pw_raw_frame *f);

// inara_pw_request_stop stops frame production (incoming buffers are
// immediately given back) and unblocks any pending inara_pw_read_raw with
// return value 1. After this call the consumer should stop pulling frames;
// frames still outstanding can still be copied and must still be released.
void inara_pw_request_stop(inara_pw *pw);

// inara_pw_destroy tears down the stream and frees the handle. Every raw
// frame handed out by inara_pw_read_raw must have been released first
// (inara_pw_request_stop makes it possible for the consumer to drain).
// Passing NULL is a no-op.
void inara_pw_destroy(inara_pw *pw);

#ifdef __cplusplus
}
#endif

#endif
