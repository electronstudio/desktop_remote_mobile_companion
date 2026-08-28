// C bridge to libpipewire for the "pipewire" video source. See
// pipewire_linux.h for the API contract. The structure follows the PipeWire
// video capture examples (and the wlrobs plugin): a pw_thread_loop runs the
// PipeWire protocol on its own thread; stream event callbacks (which run on
// that thread) stash dequeued buffers into a bounded queue; the consumer
// copies and releases them, unblocking the compositor. Frame pixel data is
// copied exactly once per frame (in inara_pw_copy_frame, on the consumer's
// thread, e.g. straight into an AVFrame) instead of once on the PipeWire
// thread and again on the consumer's.

#include "pipewire_linux.h"

#include <errno.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>
#include <spa/pod/builder.h>

struct inara_pw_raw_frame {
	struct pw_buffer *buf; // the dequeued PipeWire buffer, to give back
	const uint8_t *src;	   // pixel base (datas[0].data + chunk->offset)
	int32_t stride;		   // source row stride in bytes (chunk->stride)
	uint32_t width, height;
	enum inara_pw_format format;
};

struct inara_pw {
	struct pw_thread_loop *loop;
	struct pw_context *context;
	struct pw_core *core;
	struct pw_stream *stream;
	struct spa_hook stream_listener;

	// Negotiated raw video format; set by the param_changed event (which,
	// like process, runs on the thread-loop thread). Read only on that
	// thread and (via the negotiated_* getters) after open succeeded, when
	// negotiation is guaranteed to have happened; protected by qmutex for
	// those cross-thread reads.
	struct spa_video_info_raw info;
	enum inara_pw_format fmt;
	bool have_format;

	// Bounded frame queue: the process event pushes, inara_pw_read_raw
	// pops. When full, the oldest queued frame is dropped (its buffer is
	// returned to the stream immediately): a slow consumer should lag the
	// capture rather than stall the compositor or eat memory.
	pthread_mutex_t qmutex;
	pthread_cond_t qcond;
	inara_pw_raw_frame *queue[4];
	int qhead, qtail, qcount;
	char *error; // strdup'd stream error; set once
	bool stopping;
};

static void queue_push_locked(inara_pw *pw, inara_pw_raw_frame *f)
{
	if (pw->qcount == (int)(sizeof(pw->queue) / sizeof(pw->queue[0]))) {
		// Full: drop the oldest frame, giving its buffer straight back.
		inara_pw_raw_frame *old = pw->queue[pw->qhead];
		pw_stream_queue_buffer(pw->stream, old->buf);
		free(old);
		pw->qhead = (pw->qhead + 1) % 4;
		pw->qcount--;
	}
	pw->queue[pw->qtail] = f;
	pw->qtail = (pw->qtail + 1) % 4;
	pw->qcount++;
	pthread_cond_signal(&pw->qcond);
}

static enum inara_pw_format map_spa_format(enum spa_video_format f)
{
	switch (f) {
	case SPA_VIDEO_FORMAT_BGRx: return INARA_PW_FORMAT_BGRX;
	case SPA_VIDEO_FORMAT_BGRA: return INARA_PW_FORMAT_BGRA;
	case SPA_VIDEO_FORMAT_RGBx: return INARA_PW_FORMAT_RGBX;
	case SPA_VIDEO_FORMAT_RGBA: return INARA_PW_FORMAT_RGBA;
	case SPA_VIDEO_FORMAT_RGB: return INARA_PW_FORMAT_RGB;
	case SPA_VIDEO_FORMAT_BGR: return INARA_PW_FORMAT_BGR;
	default: return INARA_PW_FORMAT_UNKNOWN;
	}
}

static int format_bytes_per_pixel(enum inara_pw_format f)
{
	return (f == INARA_PW_FORMAT_RGB || f == INARA_PW_FORMAT_BGR) ? 3 : 4;
}

static void on_param_changed(void *userdata, uint32_t id,
			     const struct spa_pod *param)
{
	inara_pw *pw = userdata;
	if (id != SPA_PARAM_Format || param == NULL)
		return;

	struct spa_video_info_raw info;
	if (spa_format_video_raw_parse(param, &info) < 0)
		return;

	if (getenv("INARA_PW_DEBUG") != NULL)
		fprintf(stderr, "pipewire: negotiated %ux%u format=%d fps=%u/%u\n",
			info.size.width, info.size.height, info.format,
			info.framerate.num, info.framerate.denom);

	pthread_mutex_lock(&pw->qmutex);
	pw->info = info;
	pw->fmt = map_spa_format(info.format);
	pw->have_format = true;
	pthread_mutex_unlock(&pw->qmutex);
	// Wake waiters in inara_pw_open (stream events run with the loop lock
	// held, which is what pw_thread_loop_signal requires).
	pw_thread_loop_signal(pw->loop, false);
}

static void on_state_changed(void *userdata, enum pw_stream_state old,
			     enum pw_stream_state state, const char *error)
{
	inara_pw *pw = userdata;
	(void)old;
	// Verbose negotiation diagnostics, off unless debugging capture issues.
	if (getenv("INARA_PW_DEBUG") != NULL)
		fprintf(stderr, "pipewire: state %s -> %s (error=%s)\n",
			pw_stream_state_as_string(old),
			pw_stream_state_as_string(state),
			error ? error : "(null)");
	if (state == PW_STREAM_STATE_ERROR) {
		pthread_mutex_lock(&pw->qmutex);
		if (pw->error == NULL)
			pw->error = strdup(error != NULL ? error : "unknown pipewire stream error");
		pthread_cond_broadcast(&pw->qcond);
		pthread_mutex_unlock(&pw->qmutex);
	}
	pw_thread_loop_signal(pw->loop, false);
}

static void on_process(void *userdata)
{
	inara_pw *pw = userdata;
	struct pw_buffer *b = pw_stream_dequeue_buffer(pw->stream);
	if (b == NULL)
		return;

	pthread_mutex_lock(&pw->qmutex);
	if (pw->stopping || !pw->have_format ||
	    pw->fmt == INARA_PW_FORMAT_UNKNOWN) {
		pthread_mutex_unlock(&pw->qmutex);
		pw_stream_queue_buffer(pw->stream, b);
		return;
	}
	const uint32_t width = pw->info.size.width;
	const uint32_t height = pw->info.size.height;
	const enum inara_pw_format fmt = pw->fmt;
	pthread_mutex_unlock(&pw->qmutex);

	struct spa_buffer *buf = b->buffer;
	if (buf->n_datas < 1 || buf->datas[0].data == NULL) {
		pw_stream_queue_buffer(pw->stream, b);
		return;
	}
	struct spa_data *d0 = &buf->datas[0];
	struct spa_chunk *chunk = d0->chunk;
	if (chunk == NULL || chunk->stride <= 0 ||
	    chunk->offset >= d0->maxsize ||
	    chunk->offset + chunk->size > d0->maxsize) {
		// Negative strides (vertically flipped images) are rare in
		// screen capture; drop such frames rather than flipping them.
		pw_stream_queue_buffer(pw->stream, b);
		return;
	}

	inara_pw_raw_frame *f = calloc(1, sizeof(*f));
	if (f == NULL) {
		pw_stream_queue_buffer(pw->stream, b);
		return;
	}
	f->buf = b;
	f->src = (const uint8_t *)d0->data + chunk->offset;
	f->stride = chunk->stride;
	f->width = width;
	f->height = height;
	f->format = fmt;

	pthread_mutex_lock(&pw->qmutex);
	queue_push_locked(pw, f);
	pthread_mutex_unlock(&pw->qmutex);
	// The PipeWire buffer is NOT re-queued here: it now belongs to the
	// consumer until inara_pw_raw_frame_release.
}

static const struct pw_stream_events stream_events = {
	PW_VERSION_STREAM_EVENTS,
	.state_changed = on_state_changed,
	.param_changed = on_param_changed,
	.process = on_process,
};

// build_params builds the stream connection parameter: one Enumerated-format
// parameter constraining negotiation to raw video in one of our RGB formats
// (32-bit with padding/alpha, or 24-bit). Buffer data types are deliberately
// NOT restricted with a second ParamBuffers pod: with the data type
// unrestricted the compositor picks a memory type libpipewire presents
// mapped for readable memory thanks to PW_STREAM_FLAG_MAP_BUFFERS, while
// offering any restriction makes GNOME/Mutter fail negotiation with "no
// more input formats".
static void build_params(uint8_t *podbuf, size_t podbuf_size, uint32_t width,
			 uint32_t height, uint32_t framerate,
			 const struct spa_pod **params, int *n_params)
{
	struct spa_pod_builder b = SPA_POD_BUILDER_INIT(podbuf, podbuf_size);
	const struct spa_rectangle def_size = SPA_RECTANGLE(
		width != 0 ? width : 1920, height != 0 ? height : 1080);
	const struct spa_rectangle min_size = SPA_RECTANGLE(1, 1);
	const struct spa_rectangle max_size = SPA_RECTANGLE(8192, 8192);
	const struct spa_fraction def_fps =
		SPA_FRACTION(framerate != 0 ? framerate : 30, 1);
	// The ranges are deliberately permissive (down to 0/1 fps): GNOME/Mutter
	// picks from the intersection with its own offer, and a too-strict range
	// makes the node fail negotiation with "no more input formats".
	const struct spa_fraction min_fps = SPA_FRACTION(0, 1);
	const struct spa_fraction max_fps = SPA_FRACTION(1000, 1);

	params[0] = spa_pod_builder_add_object(
		&b, SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat,
		SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video),
		SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw),
		SPA_FORMAT_VIDEO_format,
		SPA_POD_CHOICE_ENUM_Id(7, SPA_VIDEO_FORMAT_BGRx,
				       SPA_VIDEO_FORMAT_BGRx,
				       SPA_VIDEO_FORMAT_RGBx,
				       SPA_VIDEO_FORMAT_RGBA,
				       SPA_VIDEO_FORMAT_BGRA,
				       SPA_VIDEO_FORMAT_RGB,
				       SPA_VIDEO_FORMAT_BGR),
		SPA_FORMAT_VIDEO_size,
		SPA_POD_CHOICE_RANGE_Rectangle(&def_size, &min_size, &max_size),
		SPA_FORMAT_VIDEO_framerate,
		SPA_POD_CHOICE_RANGE_Fraction(&def_fps, &min_fps, &max_fps));

	*n_params = 1;
}

static char *dup_printf(const char *prefix, const char *detail)
{
	char *m = malloc(strlen(prefix) + strlen(detail) + 1);
	if (m != NULL)
		(void)strcat(strcpy(m, prefix), detail);
	return m;
}

int inara_pw_open(int remote_fd, uint32_t node_id, uint32_t width,
		  uint32_t height, uint32_t framerate, int timeout_seconds,
		  inara_pw **out, char **errmsg)
{
	static pthread_mutex_t init_once_mutex = PTHREAD_MUTEX_INITIALIZER;
	static bool pw_initialized;

	*errmsg = NULL;
	*out = NULL;

	// pw_init is documented as idempotent; guard it anyway so concurrent
	// opens never double-initialize (in practice there is one streamer).
	pthread_mutex_lock(&init_once_mutex);
	if (!pw_initialized) {
		pw_init(NULL, NULL);
		pw_initialized = true;
	}
	pthread_mutex_unlock(&init_once_mutex);

	inara_pw *pw = calloc(1, sizeof(*pw));
	if (pw == NULL) {
		*errmsg = strdup("out of memory");
		return 1;
	}
	pthread_mutex_init(&pw->qmutex, NULL);
	pthread_cond_init(&pw->qcond, NULL);

	pw->loop = pw_thread_loop_new("inara-capture", NULL);
	if (pw->loop == NULL) {
		*errmsg = strdup("pw_thread_loop_new failed (is PipeWire installed?)");
		goto fail;
	}
	pw->context = pw_context_new(pw_thread_loop_get_loop(pw->loop), NULL, 0);
	if (pw->context == NULL) {
		*errmsg = strdup("pw_context_new failed");
		goto fail;
	}

	pw_thread_loop_lock(pw->loop);
	if (pw_thread_loop_start(pw->loop) < 0) {
		pw_thread_loop_unlock(pw->loop);
		*errmsg = strdup("pw_thread_loop_start failed");
		goto fail;
	}

	// The remote fd comes from the portal's OpenPipeWireRemote; duplicate
	// it because pw_context_connect_fd takes ownership.
	pw->core = pw_context_connect_fd(pw->context, dup(remote_fd), NULL, 0);
	if (pw->core == NULL) {
		pw_thread_loop_unlock(pw->loop);
		*errmsg = dup_printf("could not connect to the PipeWire remote: ",
				     strerror(errno));
		goto fail;
	}

	pw->stream = pw_stream_new(pw->core, "inara desktop capture",
				   pw_properties_new(PW_KEY_MEDIA_TYPE, "Video",
						     PW_KEY_MEDIA_CATEGORY, "Capture",
						     PW_KEY_MEDIA_ROLE, "Screen",
						     NULL));
	if (pw->stream == NULL) {
		pw_thread_loop_unlock(pw->loop);
		*errmsg = strdup("pw_stream_new failed");
		goto fail;
	}
	pw_stream_add_listener(pw->stream, &pw->stream_listener,
			       &stream_events, pw);

	uint8_t podbuf[1024];
	const struct spa_pod *params[1];
	int n_params = 0;
	build_params(podbuf, sizeof(podbuf), width, height, framerate, params,
		     &n_params);

	if (pw_stream_connect(pw->stream, PW_DIRECTION_INPUT, node_id,
			      PW_STREAM_FLAG_AUTOCONNECT |
			      PW_STREAM_FLAG_MAP_BUFFERS,
			      params, n_params) < 0) {
		pw_thread_loop_unlock(pw->loop);
		*errmsg = dup_printf("pw_stream_connect failed: ", strerror(errno));
		goto fail;
	}

	// Wait until the stream is STREAMING (format negotiated and buffers
	// flowing) or reaches an error state, with a bounded timeout. The event
	// handlers wake this loop with pw_thread_loop_signal; the deadline uses
	// the loop's own clock (which need not be CLOCK_REALTIME).
	struct timespec deadline;
	pw_thread_loop_get_time(pw->loop, &deadline,
				(timeout_seconds > 0 ? (int64_t)timeout_seconds : 15) *
				SPA_NSEC_PER_SEC);
	for (;;) {
		const char *serror = NULL;
		enum pw_stream_state state =
			pw_stream_get_state(pw->stream, &serror);
		if (state == PW_STREAM_STATE_STREAMING)
			break;
		if (state == PW_STREAM_STATE_ERROR) {
			pw_thread_loop_unlock(pw->loop);
			*errmsg = dup_printf("pipewire stream error: ",
					     serror != NULL ? serror : "unknown error");
			goto fail;
		}
		if (pw_thread_loop_timed_wait_full(pw->loop, &deadline) < 0) {
			pw_thread_loop_unlock(pw->loop);
			*errmsg = strdup("timed out waiting for the pipewire stream to start");
			goto fail;
		}
	}
	pw_thread_loop_unlock(pw->loop);

	*out = pw;
	return 0;

fail:;
	char *m = *errmsg;
	inara_pw_destroy(pw);
	*errmsg = m; // inara_pw_destroy must not clobber the message
	return 1;
}

void inara_pw_negotiated_format(const inara_pw *pw, uint32_t *width,
				uint32_t *height, enum inara_pw_format *format)
{
	inara_pw *m = (inara_pw *)pw;
	pthread_mutex_lock(&m->qmutex);
	*width = m->have_format ? m->info.size.width : 0;
	*height = m->have_format ? m->info.size.height : 0;
	*format = m->have_format ? m->fmt : INARA_PW_FORMAT_UNKNOWN;
	pthread_mutex_unlock(&m->qmutex);
}

void inara_pw_negotiated_framerate(const inara_pw *pw, uint32_t *num,
				   uint32_t *denom)
{
	inara_pw *m = (inara_pw *)pw;
	pthread_mutex_lock(&m->qmutex);
	uint32_t n = 0, d = 0;
	if (m->have_format && m->info.framerate.denom != 0 &&
	    m->info.framerate.num != 0) {
		n = m->info.framerate.num;
		d = m->info.framerate.denom;
	}
	pthread_mutex_unlock(&m->qmutex);
	*num = n;
	*denom = d;
}

void inara_pw_raw_frame_info(const inara_pw_raw_frame *f, uint32_t *width,
			     uint32_t *height, enum inara_pw_format *format,
			     int32_t *stride)
{
	*width = f->width;
	*height = f->height;
	*format = f->format;
	*stride = f->stride;
}

int inara_pw_read_raw(inara_pw *pw, inara_pw_raw_frame **out, char **errmsg)
{
	*out = NULL;
	*errmsg = NULL;

	pthread_mutex_lock(&pw->qmutex);
	for (;;) {
		if (pw->qcount > 0) {
			inara_pw_raw_frame *f = pw->queue[pw->qhead];
			pw->qhead = (pw->qhead + 1) % 4;
			pw->qcount--;
			pthread_mutex_unlock(&pw->qmutex);
			*out = f;
			return 0;
		}
		if (pw->error != NULL) {
			*errmsg = strdup(pw->error);
			pthread_mutex_unlock(&pw->qmutex);
			return 2;
		}
		if (pw->stopping) {
			pthread_mutex_unlock(&pw->qmutex);
			return 1;
		}
		pthread_cond_wait(&pw->qcond, &pw->qmutex);
	}
}

int inara_pw_copy_frame(const inara_pw_raw_frame *f, uint8_t *dst,
			int32_t dst_linesize)
{
	if (f == NULL || f->src == NULL || dst == NULL)
		return -1;
	const size_t row = (size_t)f->width *
			   (size_t)format_bytes_per_pixel(f->format);
	if (dst_linesize < (int32_t)row)
		return -1;
	for (uint32_t y = 0; y < f->height; y++)
		memcpy(dst + (size_t)y * (size_t)dst_linesize,
		       f->src + (size_t)y * (size_t)f->stride, row);
	return 0;
}

void inara_pw_raw_frame_release(inara_pw *pw, inara_pw_raw_frame *f)
{
	if (f == NULL)
		return;
	// Give the buffer back to the stream. After inara_pw_destroy the
	// stream is NULL; per the two-phase teardown contract this function is
	// always called before destroy, so the stream is live here. The lock
	// is still taken defensively.
	if (pw->stream != NULL && pw->loop != NULL) {
		pw_thread_loop_lock(pw->loop);
		if (!pw->stopping)
			pw_stream_queue_buffer(pw->stream, f->buf);
		pw_thread_loop_unlock(pw->loop);
	}
	free(f);
}

void inara_pw_request_stop(inara_pw *pw)
{
	if (pw == NULL)
		return;
	pthread_mutex_lock(&pw->qmutex);
	pw->stopping = true;
	pthread_cond_broadcast(&pw->qcond);
	pthread_mutex_unlock(&pw->qmutex);
}

void inara_pw_destroy(inara_pw *pw)
{
	if (pw == NULL)
		return;

	inara_pw_request_stop(pw);

	if (pw->loop != NULL)
		pw_thread_loop_lock(pw->loop);
	if (pw->stream != NULL)
		pw_stream_disconnect(pw->stream);
	if (pw->loop != NULL) {
		pw_thread_loop_unlock(pw->loop);
		pw_thread_loop_stop(pw->loop);
	}
	if (pw->stream != NULL)
		pw_stream_destroy(pw->stream);
	pw->stream = NULL;
	if (pw->core != NULL)
		pw_core_disconnect(pw->core);
	if (pw->context != NULL)
		pw_context_destroy(pw->context);
	if (pw->loop != NULL)
		pw_thread_loop_destroy(pw->loop);

	// Drain queued frames. Their PipeWire buffers died with the stream,
	// so (unlike inara_pw_raw_frame_release) they are just freed.
	pthread_mutex_lock(&pw->qmutex);
	while (pw->qcount > 0) {
		free(pw->queue[pw->qhead]);
		pw->qhead = (pw->qhead + 1) % 4;
		pw->qcount--;
	}
	pthread_mutex_unlock(&pw->qmutex);

	free(pw->error);
	pthread_cond_destroy(&pw->qcond);
	pthread_mutex_destroy(&pw->qmutex);
	free(pw);
}
