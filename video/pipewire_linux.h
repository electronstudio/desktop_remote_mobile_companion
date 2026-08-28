// C bridge to libpipewire for the "pipewire" video source (xdg-desktop-portal
// + PipeWire screen capture). This file and pipewire_linux.c know nothing
// about Go: the Go side (pipewire_cgo_linux.go) calls the functions below
// and the C side never calls back into Go, so no Go function pointers cross
// the boundary. Frames produced on the PipeWire thread-loop thread are
// copied into a small bounded queue (drop-oldest) that inara_pw_read pops
// from; stopping the capture unblocks a pending inara_pw_read.

#ifndef INARA_PIPEWIRE_H
#define INARA_PIPEWIRE_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// Opaque capture handle.
typedef struct inara_pw inara_pw;

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

// inara_pw_frame is one captured frame, tightly packed (stride ==
// width * bytes_per_pixel) and copied out of the PipeWire buffer. data is
// malloc'd and must be freed with inara_pw_frame_free.
typedef struct inara_pw_frame {
	uint32_t width;
	uint32_t height;
	enum inara_pw_format format;
	size_t size;
	uint8_t *data;
} inara_pw_frame;

// inara_pw_open connects to the PipeWire remote on remote_fd (duplicated
// internally; the caller keeps ownership), creates a capture stream to
// node_id, negotiates a raw video format (one of the four formats above,
// shared-memory buffers only) and blocks until the stream is STREAMING,
// which includes format negotiation. width/height (from the portal session,
// 0 if unknown) and framerate only steer the negotiation defaults; the
// actually negotiated values may differ — query them with
// inara_pw_negotiated_format.
//
// Returns 0 on success. On failure returns != 0 and sets *errmsg to a
// malloc'd message the caller must free() (unless NULL). Returns 1
// specifically when the timeout (seconds) expired while waiting for the
// stream to start.
int inara_pw_open(int remote_fd, uint32_t node_id, uint32_t width,
		  uint32_t height, uint32_t framerate, int timeout_seconds,
		  inara_pw **out, char **errmsg);

// inara_pw_negotiated_format reports the format PipeWire negotiated. It is
// valid once inara_pw_open returned 0.
void inara_pw_negotiated_format(const inara_pw *pw, uint32_t *width,
				uint32_t *height, enum inara_pw_format *format);

// inara_pw_read blocks until a frame is available, the capture is closed,
// or the stream reports an error.
//
// Returns 0 and sets *out (free with inara_pw_frame_free) on success;
// returns 1 when the capture was closed with inara_pw_close; returns 2 on a
// stream error (errmsg is a malloc'd message the caller must free()).
int inara_pw_read(inara_pw *pw, inara_pw_frame **out, char **errmsg);

// inara_pw_close tears down the stream, unblocks any pending inara_pw_read,
// and frees the handle. It is idempotent (passing NULL is a no-op).
void inara_pw_close(inara_pw *pw);

// inara_pw_frame_free releases a frame returned by inara_pw_read.
void inara_pw_frame_free(inara_pw_frame *f);

#ifdef __cplusplus
}
#endif

#endif
