# TODO

check that we are using coalesced events

investigate use of trackpad - clicks, locks - on xfce

check what happens when certificate expires


## Implementor notes

- Coordinates `x` and `y` are normalized to `[0,1]` relative to the trackpad area.
- The data channel is configured with `ordered: false, maxRetransmits: 0` for minimum latency; lost touch events are acceptable because the next event is the current state.
- The web client automatically reconnects if the WebSocket or WebRTC connection drops, with exponential back-off (1s → 2s → 4s … up to 10s).
- HTTPS is required because WebRTC APIs need a secure browser context; plain HTTP on a LAN IP is blocked.


## Device registration failure (nil virtual devices)

Root cause of a reported segfault (`signaling.(*Session).handleDataMessage`
nil pointer dereference, followed by `Failed to execute with sudo: exit
status 2`): when `trackpad.New()`/`tablet.New()` fails, `server.New` calls
`reExecWithSudo`, which is a no-op when `cli.DontRunSudo` is set -- i.e. in
the sudo-re-exec'd child itself, and in the GUI (`cmd/inara_gui` sets it
unconditionally). Startup then continues with a nil device interface in the
session route map. A defensive nil guard in `handleDataMessage` now prevents
the crash (events are logged and dropped), but the underlying handling is
still open:

- Fail fast on device-creation failure: when `reExecWithSudo` cannot help
  (`--dont-run-sudo` child or GUI), `server.New` should return an error /
  exit instead of serving with nil devices. The nil devices can still reach
  other unguarded paths: `resetProcessors` calls `Reset()` on every
  processor, and `Shutdown` calls `Close()` on `s.pad`/`s.tablet`.
- Better diagnostics at registration failure: if running as root and opening
  `/dev/uinput` fails with ENOENT, the uinput kernel module is not loaded --
  print actionable advice (`sudo modprobe uinput`; persist via
  `echo uinput | sudo tee /etc/modules-load.d/uinput.conf`; if the kernel
  was just upgraded, reboot first -- the running kernel's module directory
  is gone, so modprobe fails until reboot; this is the likely reason the
  reported crash was "fixed by a reboot").
- Optionally, when registration fails and we are already root, attempt
  `modprobe uinput` automatically before giving up.
- Optionally, degraded operation instead of fail-fast: build the processors
  map only from successfully created devices and surface "device X disabled"
  in the web UI status (less attractive -- both devices are core features).

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
