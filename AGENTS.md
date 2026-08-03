# AGENTS.md — desktop_remote_mobile_companion

This file contains the context coding agents need to work on this project.

# Version numbers

Always increment the minor version number sored in VERISON file before doing a build, e.g. server/VERSION 0.3.7 becomes 0.3.8.

# Packaging

`.github/workflows/build.yml` builds Linux (x86_64) and Windows artifacts and also produces a Debian/Ubuntu `.deb` in the `build-linux` job: the package tree mirrors `make install` (`companion` + `companion_gui` in `/usr/bin`, desktop file, hicolor icon, LICENSE as copyright), `Depends:` is generated with `dpkg-shlibdeps` (FFmpeg is statically linked), and `packaging/deb/postinst` applies the `setcap cap_sys_admin,cap_dac_override,cap_setpcap=p` grant to both binaries at install time (as the Makefile does). The control-file/postinst templates live in `packaging/deb/`. The `.deb` is uploaded as its own artifact and also copied uncompressed into the combined "all" artifact by the `combine` job.

## Project overview

This project turns a mobile device into a multitouch trackpad/remote input device for a Linux desktop PC. The Go program:

1. Serves a single-page web app over HTTPS.
2. Provides WebSocket-based WebRTC signaling.
3. Receives pointer events from the phone over a WebRTC data channel.
4. Prints the events to the terminal as raw JSON.
5. Emits the events as either a Linux virtual multitouch trackpad or an absolute graphics tablet via `uinput`.
6. Displays the embedded version number in both the server log and the web client.

## Module

```
github.com/electronstudio/desktop_remote_mobile_companion
```

## Architecture

```
+-----------+   HTTPS/WSS    +-----------------+   WebRTC data channel   +-----------+
|   Phone   | <-----------> |  Go server      | <-----------------------> |   Linux   |
| browser   |                |  - static files |                         |  desktop  |
|           |                |  - /signal ws   |                         |  terminal |
+-----------+                |  - pion/webrtc  |                         +-----------+
                             +-----------------+
```

- `cmd/companion/main.go` — the `companion` CLI binary entry point: parses command-line flags (via `go-arg`) and calls `server.Run`.
- `cmd/companion_gui/main.go` — the `companion_gui` binary entry point: a Fyne GUI with a "Start" button that runs `server.Run` in a goroutine with default flags (no CLI parsing), plus a read-only scrollable log text area that mirrors everything written via the standard `log` package (any package) through a process-global `log.SetOutput(io.MultiWriter(os.Stderr, ...))` redirect; a bounded line buffer + `fyne.Do` flush goroutine keeps the widget updated thread-safely.
- `server/server.go` — the shared server library: HTTPS server, self-signed certificate generation, static/index serving, virtual-device creation, and version injection, exposed as `server.Run(cli CLI)`. The `/signal` WebSocket handler upgrades the connection and delegates to a `signaling.Session`, so `server.go` no longer contains WebRTC or device-routing logic.
- `server/platform_linux.go` — platform/capability helpers (`hasCapSysAdmin`, `onNoSuidMount`, `reExecWithSudo`, privilege dropping), moved into the `server` package.
- `server/platform_windows.go` — Windows stubs of the platform helpers.
- `server/device_help.go` — the uinput/CAP_SYS_ADMIN instruction strings shown to the user on permission errors.
- `signaling/session.go` — WebRTC signaling protocol for one client connection: the WebSocket signaling loop, peer-connection lifecycle, data-channel event routing to virtual input devices via an `input.EventProcessor` route map, and the desktop-video pipeline (built on demand from the offer, fail-open, in `maybeAddVideoTrack`).
- `input/event.go` — shared `Touch`/`Event` types and coordinate helpers used by both devices.
- `input/processor.go` — `EventProcessor` and `Activator` interfaces the signaling layer routes data-channel events through.
- `trackpad/trackpad_linux.go` — virtual Linux multitouch trackpad (Stage 2).
- `tablet/tablet_linux.go` — virtual Linux absolute graphics tablet.
- `video/video_linux.go` — Linux kmsgrab capture backend (DRM framebuffer) feeding a Pion H264 WebRTC track.
- `video/video_x11grab_linux.go` — Linux x11grab capture backend (X server).
- `video/encoder.go` — shared H264 encoder + filter-graph helper (the orthogonal "encoder axis": h264_vaapi / h264_nvenc / libx264), used by all capture backends.
- `video/video_windows.go` — Windows ddagrab capture backend (placeholder pending implementation).
- `server/static/index.html` — responsive touch UI with mode selector, embedded desktop `<video>` in the tablet panel, and version display.
- `server/static/app.js` — browser WebRTC client, touch-event capture, mode switching, recv-only video transceiver + `ontrack` rendering with visibility gating.
- `server/VERSION` — embedded version string, displayed by both server and client.

## Build and run

```bash
# default port 8080
go run ./cmd/companion

# custom port
go run ./cmd/companion --port 8443
# or
go run ./cmd/companion -p 8443

# GUI (opens a Fyne window with a Start button; ignores CLI args)
go run -tags migrated_fynedo ./cmd/companion_gui
```

Build the binaries:

```bash
go build -o companion ./cmd/companion
go build -tags migrated_fynedo -o companion_gui ./cmd/companion_gui
```

The static HTML/JS assets are embedded into the binary using `//go:embed` (from the `server` package), so each compiled binary is self-contained. The GUI binary links Fyne (via GLFW), which needs the X11/Wayland/OpenGL client libraries at runtime (`libglvnd`, `mesa`, `libx11`, `libxcursor`, `libxrandr`, `libxinerama`, `libxi`, `wayland` on Arch; `libgl1`, `libegl1`, `libwayland-*`, `libx11`, `libxcursor1`, `libxrandr2`, `libxinerama1`, `libxi6` on Debian).

## Configuration

Command-line flags are handled by `github.com/alexflint/go-arg`.

| Flag | Default | Description |
|---|---|---|
| `-p`, `--port` | `8080` | HTTPS listen port |
| `--video-source` | `kmsgrab` | Desktop video capture source: `kmsgrab` (DRM framebuffer), `x11grab` (X server), or `none` to disable video. On Windows the source is always `ddagrab` regardless of this flag |
| `--video-encoder` | `auto` | Video H264 encoder: `vaapi`, `nvenc`, `libx264`, or `auto` (auto = nvenc on NVIDIA, else vaapi, else libx264). `libx264` is only used when manually specified or as a last-resort fallback |
| `--video-card` | (auto) | DRM card to capture (e.g. `/dev/dri/card1`); empty auto-detects the first `/dev/dri/card*` (kmsgrab only) |
| `--video-fps` | `30` | Video capture frame rate |
| `--video-qp` | `24` | Encoder quality (h264_vaapi/h264_nvenc QP, or libx264 CRF; lower is higher quality) |
| `--low-power` | `0` | h264_vaapi low-power mode (0 or 1); ignored for other encoders |
| `--video-width` | `0` | Cap output width; `0` = native (reserved for future downscaling) |
| `--no-tablet-keepalive` | `false` | Disable the tablet hover keep-alive (a GNOME/Mutter cooldown workaround). On compositors without that cooldown (e.g. wlroots/Sway), set this so the system mouse is not grabbed while the tablet panel is idle |

Example:

```bash
./companion --port 8443
```

## TLS certificate

The server needs HTTPS because browser WebRTC APIs require a secure context. A self-signed certificate is generated automatically on first run and reused afterwards.

- Storage location: `os.UserCacheDir()/desktop_remote_mobile_companion/`
  - Linux: `$HOME/.cache/desktop_remote_mobile_companion/`
  - macOS: `$HOME/Library/Caches/desktop_remote_mobile_companion/`
  - Windows: `%LocalAppData%\desktop_remote_mobile_companion\`
- Files: `server.crt`, `server.key`
- The certificate includes `localhost`, `127.0.0.1`, `::1`, and all non-loopback local interface IPs in the SAN list.
- The SHA-256 fingerprint is printed at startup so users can verify it on first visit.
- Delete the two files to force regeneration.

## uinput permissions

Creating a virtual input device requires write access to `/dev/uinput`.

Add a udev rule, then reload:

```bash
echo 'KERNEL=="uinput", MODE="0660", GROUP="input"' | sudo tee /etc/udev/rules.d/99-uinput.rules
sudo udevadm control --reload
sudo udevadm trigger
```

Make sure your user is in the `input` group (`usermod -aG input $USER`).

## Desktop video capture permissions (CAP_SYS_ADMIN)

The `kmsgrab` DRM demuxer used by the video pipeline requires `CAP_SYS_ADMIN` to acquire DRM master and map the framebuffer. Without it every phone connection fails with a cryptic FFmpeg error:

```
[kmsgrab @ 0x...] No handle set on framebuffer: maybe you need some additional capabilities?
video: open kmsgrab /dev/dri/cardN: Invalid argument
```

At startup, when the source is `kmsgrab` (the default, i.e. not `--video-source=none`), the server checks the process effective capability set (bit 21 of `CapEff` from `/proc/self/status`). If `CAP_SYS_ADMIN` is missing it logs a clear warning with the fix and auto-disables video for that run; trackpad/tablet keep working and the per-connection FFmpeg error is never printed.

To grant the capability to the binary once (no need to run as root afterwards):

```bash
sudo setcap cap_sys_admin+ep ./companion
```

`setcap` is stored as a file extended attribute, so it must be re-applied after every rebuild (the file is replaced). Alternatively run with `--video-source=none` to use only the trackpad/tablet. Running the binary as root also works but is not recommended.

### File capabilities and nosuid mounts

`setcap` only takes effect on filesystems mounted **without** `nosuid`. The `nosuid` mount option silently disables file capabilities: `setcap` writes the `security.capability` xattr (so `getcap` shows the cap) but the kernel ignores it at exec time, so the process still lacks `CAP_SYS_ADMIN`. The startup check detects this (via `statfs` + `ST_NOSUID` on the executable's path) and prints a nosuid-specific message instead of the generic `setcap` advice.

If your binary lives on a `nosuid` mount (common for encrypted/home partitions such as `/encrypted`, `/home`, or a separate `/usr` with `nodev,nosuid`), either:

- Copy/move the binary to a non-`nosuid` location (e.g. `/usr/local/bin`, `/opt`, `/tmp`) and `setcap` there, or
- Remount that filesystem with `suid` (add `suid` to its mount options in `/etc/fstab` or mount it with `suid`), then re-run `setcap`, or
- Run the program as root (`sudo`), which has `CAP_SYS_ADMIN` in its bounding set regardless of `nosuid`.

Check a mount's options with `findmnt <path>` or `mount | grep <path>`.

## Tablet axis resolution

The virtual graphics tablet advertises `ABS_X`/`ABS_Y` resolution of 200 units/mm. libinput rejects tablets with zero resolution ("missing tablet capabilities: resolution") and ignores the device for the whole session, so the resolution must be visible to libinput at probe time.

The device sets the full absinfo (including resolution) **before** `UI_DEV_CREATE` using the `UI_ABS_SETUP` ioctl (Linux 4.16+) via our local fork of `github.com/jbdemonte/virtual-device` in `third_party/virtual-device/`. The device is therefore born with the correct resolution and libinput sees it on the first probe — no udev hwdb entry is required.

### Optional legacy hwdb entry (pre-4.16 kernels only)

On kernels older than 4.16, `UI_ABS_SETUP` is unavailable and the resolution must be set at device-creation time via a udev hwdb entry instead:

```bash
sudo tee /etc/udev/hwdb.d/60-desktop-remote-mobile-companion.hwdb > /dev/null << 'EOF'
# Spoofed pen tablet (virtual uinput device)
evdev:input:b0003v056Ap0301*
 EVDEV_ABS_00=::200
 EVDEV_ABS_01=::200
EOF

sudo systemd-hwdb update 2>/dev/null || sudo udevadm hwdb --update
```

This matches our virtual device (bus `0x0003` = `BUS_USB`, vendor `0x056a` = Wacom, product `0x0301` = One by Wacom medium — see "Graphics tablet mode") and sets `ABS_X`/`ABS_Y` resolution to 200 units/mm. On modern kernels (4.16+) this entry is redundant because `UI_ABS_SETUP` already sets the resolution at creation time.

## WebRTC signaling flow

1. Browser opens `wss://<host>/signal`.
2. Browser creates an `RTCPeerConnection` + `RTCDataChannel('touch')`.
3. Browser sends `offer` SDP.
4. Server sets remote description, creates `answer`, sets local description, sends `answer`.
5. Both sides exchange trickle `candidate` messages as ICE candidates are discovered.
6. Once the data channel opens, the browser streams pointer events to the server.
7. If the WebSocket or WebRTC connection is lost, the client automatically closes the old peer connection and creates a new one, with exponential back-off (1s → 2s → 4s … max 10s).

## Pointer event format

The browser uses `PointerEvent` (instead of `TouchEvent`) because `pointermove` exposes `getCoalescedEvents()`, giving higher-frequency sub-frame samples than raw touch events. Each pointer sample produces one JSON message sent over the data channel:

```json
{
  "device": "trackpad",
  "type": "pointermove",
  "w": 412.5,
  "h": 380.25,
  "t": [
    {"id": 1, "x": 152.3, "y": 193.8}
  ]
}
```

- `device`: one of `trackpad` or `tablet`. The server routes the event to the corresponding virtual input device. A client may send events for both devices, even interleaved.
- `type`: one of `pointerdown`, `pointermove`, `pointerup`, `pointercancel`, `buttondown`, `buttonup`. The server maps pointer events to the same lifecycle as touch events. Button events are routed to the tablet device only.
- `button`: required for `buttondown`/`buttonup`; one of `left`, `middle`, `right`.
- `w`, `h`: the panel's CSS-pixel size at send time (`getBoundingClientRect().width/height`). Fractional on HiDPI screens/fractional layouts; omitted for button events.
- `t`: array containing a single pointer sample (omitted for button events).
- `id`: pointer identifier (`PointerEvent.pointerId`).
- `x`, `y`: RAW panel-relative CSS-pixel coordinates (`clientX - rect.left`, `clientY - rect.top`) — NOT normalized. They are fractional on HiDPI screens and may lie outside `[0,w]`/`[0,h]` when a captured pointer is dragged off the panel; the server normalizes against `w`/`h` and clamps. For the tablet the server additionally remaps for the video's `object-fit: contain` letterboxing (see "Graphics tablet mode").

The WebRTC data channel is configured as ordered and reliable (`{ ordered: true }`) so events arrive in order and are not dropped under load.

The server prints the raw JSON line to stdout using `fmt.Printf`.

## Implementation notes

- **Pointer events vs. touch events**: the browser uses `PointerEvent` because `pointermove` exposes `getCoalescedEvents()`. For the trackpad we send only the latest coalesced sample per `pointermove` frame; sending every sub-frame sample flooded the data channel and produced inconsistent pointer motion in libinput. For the tablet we send the dispatched (non-coalesced) `pointermove` event instead, because coalesced `PointerEvent`s report `pressure`/`tiltX`/`tiltY` as 0 on some platforms — using them collapsed the pen pressure to 0 immediately after `pointerdown` and lifted the tip mid-stroke.
- **Axis range/resolution**: the virtual touchpad advertises `ABS_X`/`ABS_Y`/`ABS_MT_POSITION_*` with a range of `0..8191` and resolution `80 units/mm`. This matches a roughly 102 mm real trackpad. Larger ranges such as `0..32767` / `320 units/mm` made the cursor too slow and caused libinput's acceleration curve to behave inconsistently.
- **Single-touch axes during multitouch**: `ABS_X`/`ABS_Y`/`ABS_PRESSURE` are only emitted when exactly one contact is active. With two or more contacts the device emits only MT events, which lets libinput classify two-finger scroll and pinch gestures correctly.
- **Clickpad button model**: the device only advertises `BTN_LEFT` plus the `INPUT_PROP_BUTTONPAD` property. Real clickpads do not advertise a physical `BTN_RIGHT`; that should be software-emulated.
- **Graphics tablet mode**: the tablet device mimics a real pen tablet. To appear in the GNOME Settings "Graphics Tablets" panel it spoofs a **One by Wacom (medium)** (CTL-671, `usb:056a:0301`) — a pen-only tablet with no pad buttons, no touch and pressure+tilt, matching the capabilities we emulate. GNOME only lists tablets libwacom can describe, and libwacom's `get_device_info()` rejects a `BUS_VIRTUAL` (0x06) device ("Unsupported bus 'unknown'") because it cannot map the bus and finds no `UINPUT_SUBSYSTEM` udev property, so even GNOME 47+'s generic fallback never shows it; spoofing a real USB vendor/product makes libwacom resolve it (`libwacom-list-local-devices` shows it). The device uses `BTN_TOOL_PEN` for proximity, `ABS_X`/`ABS_Y` (0..32767, 200 units/mm), `ABS_PRESSURE` (0..4096), `ABS_DISTANCE` (0..255), `ABS_TILT_X`/`ABS_TILT_Y` (-90..90, degrees), and `ABS_MISC` for tool tracking via `MSC_SERIAL`. Touching the surface is a pen-tip contact: the server emits proximity-in (hover) then `BTN_TOUCH=1` with `distance=0` and the pen's live pressure, so the compositor moves the cursor and applications draw — pressure and tilt are forwarded from the browser `PointerEvent` (`pressure`, `tiltX`, `tiltY`) and only for the tablet device. The pen's raw panel coordinates are remapped for the letterboxing the client adds with `object-fit: contain` (see below): the server reads the desktop capture resolution from `video.CaptureWidth`/`CaptureHeight` and, when its aspect ratio differs from the panel's, offsets+scales the coordinate onto the visible image (touches in the black bars clamp to the desktop edge), so the pen tracks the desktop picture. With no active video (`--video-source=none`) the mapping is the identity and the tablet spans the whole desktop. libinput derives tablet tip-down from `ABS_PRESSURE` crossing a threshold (it ignores `BTN_TOUCH` when an `ABS_PRESSURE` axis exists), so while the tip is down the server floors the emitted `ABS_PRESSURE` at `tipFloor` (256, above libinput's ~5% default) to keep the tip logically down through the low/spurious pressure samples a stylus reports at first touch and during light contact. Lifting the finger/stylus releases the tip back to a hover but **stays in proximity** (a real pen keeps hovering between strokes). Because a phone touchscreen reports no real hover, the server would otherwise go silent between strokes; libinput's no-proximity-out quirk then forces a proximity-out, and the resulting proximity-out→in cycle triggers a GNOME/Mutter (Wayland) multi-second cooldown during which the tool's pointer is not delivered (a stroke does nothing for a few seconds, then starts drawing mid-way). To prevent that, the server runs a **keep-alive ticker** (default on; `--no-tablet-keepalive` to disable) that re-emits the hover frame every ~15 ms while the tool is hovering (in range, tip up). The frame toggles `ABS_DISTANCE` between two values because the Linux input protocol deduplicates unchanged axis values — identical frames produce zero evdev events. This keeps the tool "alive" so the quirk never fires. Because the keep-alive keeps the tool perpetually in proximity, the system mouse would be grabbed while the tablet panel is active; the client avoids this by sending an `activate` control message when the tablet panel becomes (in)active — swiping away to another panel triggers a clean proximity-out (`Reset`), releasing the mouse, and the next touch on the tablet panel does a fresh proximity-in (a lone first stroke, which Mutter handles). A client disconnect also proximity-outs cleanly. On compositors without the Mutter cooldown (e.g. wlroots/Sway), pass `--no-tablet-keepalive` to disable the perpetual hover entirely (the mouse works and strokes work without the keep-alive). The axis resolution is set at device-creation time via `UI_ABS_SETUP` (Linux 4.16+) — see "Tablet axis resolution" below.

## Desktop video streaming

The phone's **tablet** panel shows a live H264 stream of the PC desktop, sent server→browser over the same WebRTC peer connection as the touch data channel (opposite direction). The browser adds a `recvonly` video transceiver; when the server's `signaling.Session` (`maybeAddVideoTrack`) sees a video m-line in the offer it builds a capture pipeline, `AddTrack`s an H264 `TrackLocalStaticSample`, and answers. H264 samples flow over RTP to the phone's `<video>` element.

The capture pipeline has two independent axes: the **source** (`--video-source`) and the **encoder** (`--video-encoder`, auto by default). On Linux the kmsgrab source (the default) mirrors:

```
ffmpeg -device /dev/dri/card0 -f kmsgrab -i - \
    -vf 'hwmap=derive_device=vaapi,scale_vaapi=format=nv12' \
    -c:v h264_vaapi -qp 24 -bf 0 -
```

The `x11grab` source reads pixels from the X server (no `CAP_SYS_ADMIN` needed) and pairs with any encoder (libx264 software, or h264_vaapi/h264_nvenc via `hwupload`). The source owns the input + decode pipeline (`video_linux.go` / `video_x11grab_linux.go`); the shared `encoder.go` owns the filter graph + H264 encoder, chosen from the source's frame type (hardware vs software) and the requested encoder family. The auto encoder default is h264_nvenc on NVIDIA systems (except kmsgrab, which cannot feed nvenc), else h264_vaapi if available, else libx264.

Notes:
- **Encoder availability.** h264_vaapi needs a VAAPI-capable GPU; h264_nvenc needs the NVIDIA driver; libx264 is pure software. The auto default falls back to libx264 only when no hardware encoder is available (or when manually specified).
- **kmsgrab requires `CAP_SYS_ADMIN`** (see below) and typically has no VAAPI on NVIDIA, so on NVIDIA systems the server warns and `x11grab` is the better source.
- **x11grab on Wayland** only captures the X server (XWayland), so native Wayland surfaces may appear as a black screen; the server warns and `kmsgrab` is the better source on Wayland.
- **Build needs** the FFmpeg/libdrm C dev packages (`libavcodec-dev libavfilter-dev libavformat-dev libavutil-dev libavdevice-dev libdrm-dev`), x11grab support in FFmpeg, and CGO.
- **Graceful degradation.** If `video.New` fails (no encoder/source/driver), the server logs a warning, adds no video track, and trackpad/tablet keep working; the tablet panel shows its placeholder.
- **Visibility gating.** The browser only attaches the track to a `<video>` while the tablet panel is the active panel in its area, so a hidden surface does no decode work.
- **One client at a time.** The pipeline is created per peer connection; `kmsgrab` is exclusive, so only one phone receives video concurrently. A shared fan-out pipeline is future work (see `docs/improvements.md`).
- The fixed 30 fps default, a Windows ddagrab backend, adaptive native framerate, and multi-client fan-out are tracked in `docs/improvements.md`.

## Dependencies

Key Go modules:

- `github.com/pion/webrtc/v4` — WebRTC peer connection and data channel.
- `github.com/gorilla/websocket` — WebSocket signaling server.
- `github.com/alexflint/go-arg` — command-line argument parsing.
- `github.com/jbdemonte/virtual-device` — virtual Linux input devices via `uinput`.
- `github.com/asticode/go-astiav` — FFmpeg/libav C bindings for desktop capture (kmsgrab) and VAAPI H264 encoding.

No JavaScript build step is used; `static/app.js` is served as-is.

## Development conventions

- Keep changes minimal and focused on the current stage.
- Follow existing Go formatting (`gofmt`).
- Do not commit generated files (`server.crt`, `server.key`, binary). They are `.gitignore`d.
- Static assets must remain embeddable; avoid generating files at runtime that the binary depends on.

## Testing

Manual test flow:

1. `go run ./cmd/companion`
2. Open the printed LAN URL in a phone browser.
3. Accept the self-signed certificate.
4. Verify the status area shows “Data channel open” and the version number.
5. Touch the top half of the screen and confirm JSON lines appear in the desktop terminal.
6. Verify the virtual devices are registered:
   - `ls /dev/input/by-id/` or `evtest` should show devices named “Desktop Remote Mobile Companion Touchpad” and “One by Wacom (medium) Pen”.
   - For the trackpad, run `evtest /dev/input/event<N>` and confirm `EV_ABS / ABS_MT_POSITION_X`, `ABS_MT_POSITION_Y`, and `ABS_MT_SLOT` events are emitted for multitouch contacts.
   - For the tablet, run `evtest` and confirm `EV_ABS / ABS_X` and `ABS_Y` events are emitted as an absolute single-point contact.
   - Alternatively, `libinput debug-events` shows pointer, multitouch, and tablet-tool events.
7. Switch the mode selector to “Tablet”, touch the phone, and confirm the cursor follows your finger as an absolute position.
8. With video enabled (default `--video-source=kmsgrab`), switch to the “Tablet” panel and confirm the desktop appears in the `<video>` element. Swipe away to another panel and confirm the video stops rendering; swipe back and confirm it resumes. Try `--video-source=x11grab` (with a software `--video-encoder=libx264`, or auto on a system with VAAPI/NVIDIA) and confirm it captures the X desktop. Run with `--video-source=none` and confirm the tablet panel shows only the “Tablet” placeholder and trackpad/tablet still work.

Automated headless Chromium checks are possible with `--ignore-certificate-errors` and `--virtual-time-budget`, but WebRTC connection setup timing is flaky in headless mode; prefer manual device testing.

## Future stages

- Stage 2: translate touch events into Linux mouse/touchpad input via `uinput`. ✅
- Stage 3: multitouch gestures (two-finger scroll, pinch, etc.).
- Stage 4: optional pairing/authentication, connection reliability, and UI polish.
