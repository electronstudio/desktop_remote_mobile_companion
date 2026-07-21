# Improvements (future work)

## Video capture fallback

- Software fallback path for machines without a VAAPI-capable GPU: `x11grab`
  (or `x11grab` via `pipewire`/`dmabuf`) as the capture source plus `libx264`
  software encoder, replacing the kmsgrab + h264_vaapi pipeline. Detect VAAPI
  availability at runtime and fall back automatically, or add a
  `--video-encoder {vaapi,x264}` flag.

## Adaptive framerate

- Currently fixed at 30 fps. The capture loop should use the native frame rate
  of the desktop/display (kmsgrab exposes this; for other sources read the
  stream's time base). Also add a `--video-fps` command-line override that
  takes precedence over the detected native rate.

## Other potential work

- Multi-client fan-out: a single shared capture pipeline distributing H264
  samples to N peer connections (kmsgrab is exclusive today, so only one phone
  at a time gets video).
- Adaptive bitrate: scale `qp` (or switch to a bitrate-based rate control)
  based on the WebRTC transport-cc / bandwidth estimation so the stream does
  not saturate a weak Wi-Fi link.
- Cursor / window selection: capture a specific output/monitor or a specific
  window rather than the full DRM framebuffer.
- Audio: optional companion audio track alongside the video.
