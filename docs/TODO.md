# TODO

check that we are using coalesced events

investigate use of trackpad - clicks, locks - on xfce

check what happens when certificate expires


## Implementor notes

- Coordinates `x` and `y` are normalized to `[0,1]` relative to the trackpad area.
- The data channel is configured with `ordered: false, maxRetransmits: 0` for minimum latency; lost touch events are acceptable because the next event is the current state.
- The web client automatically reconnects if the WebSocket or WebRTC connection drops, with exponential back-off (1s → 2s → 4s … up to 10s).
- HTTPS is required because WebRTC APIs need a secure browser context; plain HTTP on a LAN IP is blocked.


## Maybe

- Concurrent-client policy. Every `/signal` WebSocket is currently allowed
  (the server session registry exists only for graceful shutdown). All
  sessions share the two virtual input devices -- simultaneous touches from
  two phones interleave into one device, and one client disconnecting resets
  (lifts) state the other may be using; video is exclusive to the first
  capture pipeline and fails open for later ones. Even a single client page
  reload can race: if the new offer arrives before the old pipeline finishes
  stopping, the new session's `video.New` fails open (no video until the
  next reconnect). Options, now easy to build on the registry:
  (a) reject a second concurrent `/signal` with 409, or
  (b) "take over": a new `/signal` cleanly closes the existing session(s),
  which fixes both the multi-phone confusion and the reload race.

- Multi-client fan-out: a single shared capture pipeline distributing H264
  samples to N peer connections (kmsgrab is exclusive today, so only one phone
  at a time gets video?).
- Windows GPU vendor detection so `--video-encoder=auto` on Windows can
  prefer a native hardware encoder (h264_nvenc / h264_amf) over h264_mf.
  Auto already resolves to h264_mf, the Media Foundation transform Windows
  picks for the primary adapter (typically Intel Quick Sync); the ddagrab
  backend also supports nvenc and amf, but auto cannot tell which GPU vendor
  is present.

- Multi-monitor for ddagrab: capture `output_idx` other than 0 (0 = the
  primary display only, matching current behaviour).

- h264_amf + ddagrab without `scale_d3d11` (AMF derives its own D3D11 device
  context, but ddagrab's frames are on the shared D3D11 device; the
  `scale_d3d11` video-processor filter fails on WARP / feature-level-9
  drivers, so today h264_amf only works on discrete AMD setups).
