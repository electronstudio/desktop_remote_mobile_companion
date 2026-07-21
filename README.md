# Desktop Remote Mobile Companion

Connect your mobile device wirelessly to your PC and use it as:

* Trackpad
* Graphics tablet
* Display mirroring (Intel/AMD only)
* ~~Camera~~ (TO DO)
* ~~Microphone~~ (TO DO)
* ~~Keyboard~~ (TO DO)
* ~~Clipboard~~ (TO DO)
* ~~Extra display~~ (TO DO)
* ~~Game controller~~ (TO DO)

Designed for Linux Wayland.  Linux X11 may work.

## Compared to Weylus

[Weylus](https://github.com/electronstudio/WeylusCommunityEdition) is a program for graphic tablet
and display mirroring.  DRMC explores some _different_ methods of implementing those features,
which may not necessarily be better!

DRMC uses WebRTC connections. The underlying protocol is UDP rather than TCP which _may_ (or may not)
have lower latency.

DRMC mirrors the display using ffmpeg's kmsgrab, which _may_ (or may not) work better on Wayland (although doesn't
work at all on Nvidia systems).

DRMC uses hardware accelerated VAAPI encoding.

DRMC is written in Go rather than Rust.

## Build

```bash
go build
```

Desktop video streaming requires VAAPI and the FFmpeg/libdrm C development
libraries. On Debian/Ubuntu:

    sudo apt install libavcodec-dev libavfilter-dev libavformat-dev \
        libavutil-dev libavdevice-dev libdrm-dev

It also requires a VAAPI-capable GPU and access to the DRM device
(`/dev/dri/card*`). The capture pipeline uses the `kmsgrab` demuxer and the
`h264_vaapi` hardware encoder. If video is unavailable the trackpad and
tablet still work; the tablet panel just shows a placeholder.

## Install

From releases.

Needs uinput group permissions.  Enter this command then reboot:

    sudo usermod -aG uinput $USER

To enable desktop video capture, grant CAP_SYS_ADMIN to the binary once (no need to run as root afterwards):

    sudo setcap cap_sys_admin+ep desktop_remote_mobile_companion

## Run

```bash
./desktop_remote_mobile_companion --port 8080
```

Video streaming flags:

| Flag | Default | Description |
|---|---|---|
| `--no-video` | off | Disable desktop video streaming entirely. |
| `--video-card` | auto | DRM card to capture (`/dev/dri/card1`); empty auto-detects. |
| `--video-fps` | `30` | Video capture frame rate. |
| `--video-qp` | `24` | h264_vaapi constant-quality QP (lower = higher quality). |
| `--low-power` | `1` | h264_vaapi low-power mode (0 or 1). |
| `--video-width` | `0` | Cap output width; `0` = native  |

The first time it runs, a self-signed certificate is created in your user cache
directory (`$HOME/.cache/desktop_remote_mobile_companion` on Linux).

It will print a URL and also a QR code for that URL, e.g. `https://192.168.1.150:8080`. 
  Open this URL on your mobile device.  You will need to accept the self-signed certificate.

Add a bookmark to your homescreen to access the app fullscreen and allow
swipes to work properly.

Swipe left/right from screen edge to switch tools.  Rotate
device to landscape orientation to use tool fullscreen.

## Implementor notes

- Coordinates `x` and `y` are normalized to `[0,1]` relative to the trackpad area.
- The data channel is configured with `ordered: false, maxRetransmits: 0` for minimum latency; lost touch events are acceptable because the next event is the current state.
- The web client automatically reconnects if the WebSocket or WebRTC connection drops, with exponential back-off (1s → 2s → 4s … up to 10s).
- HTTPS is required because WebRTC APIs need a secure browser context; plain HTTP on a LAN IP is blocked.
