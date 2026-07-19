# Desktop Remote Mobile Companion

Connect your mobile device wirelessly to your PC and use it as:

* Trackpad (DONE)
* Graphics tablet (IN PROGRESS)
* Display (TO DO)
* Camera (TO DO)
* Microphone (TO DO)
* Keyboard (TO DO)
* Clipboard (TO DO)

Designed for Linux Wayland.  Linux X11 may work.  Windows TODO.

## Build

```bash
go build
```

## Install

From releases.

Needs uinput group permissions.  Enter this command then reboot:

    sudo usermod -aG uinput $USER

## Run

```bash
./desktop_remote_movile_companion --port 8080
```

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
