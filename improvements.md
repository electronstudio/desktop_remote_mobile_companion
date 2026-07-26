# Improvements (future work)

## Video capture / encoder selection

- Implemented: the `--video-source` flag selects the capture backend
  (`kmsgrab` or `x11grab` on Linux), and `--video-encoder` selects the H264
  encoder (`h264_vaapi`, `h264_nvenc`, `libx264`, or `auto`). The encoder axis
  is shared across sources in `video/encoder.go`; `x11grab` pairs with any
  encoder (libx264 software, or vaapi/nvenc via `hwupload`). Auto-encoder
  falls back to `libx264` when no hardware encoder is available.
- Still future: **automatic** kmsgrab→x11grab fallback when the chosen source
  fails at runtime (today the user picks the source manually; a failed
  attempt degrades to no-video rather than retrying the other source).
- Still future: a **Windows ddagrab** capture backend (`video_windows.go` is a
  not-yet-implemented stub; on Windows video is disabled until it lands).

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
