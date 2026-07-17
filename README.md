# Desktop Remote Mobile Companion — Stage 1

A tiny Go server that turns a phone/tablet into a multitouch trackpad for a Linux desktop. Touch events on the phone's screen are sent over a WebRTC data channel (UDP-based, low latency) to the desktop and printed to the terminal.

## What it does in Stage 1

- Serves a single-page web app over HTTPS on port `8080` by default.
- Automatically generates a self-signed TLS certificate on first run.
- The browser creates a WebRTC peer connection and data channel.
- The top half of the phone screen acts as a trackpad.
- Every `touchstart`, `touchmove`, `touchend`, and `touchcancel` event is sent to the server as JSON and printed to stdout.

## Requirements

- Go 1.21 or newer
- A modern mobile browser (Chrome, Safari, Firefox, Edge)

## Build / run

```bash
cd /home/richard/git_temp/desktop_remote_mobile_companion
go run .
```

Use a different port:

```bash
go run . --port 8443
```

The first time it runs, a self-signed certificate is created in your user cache directory (`$HOME/.cache/desktop_remote_mobile_companion` on Linux). The server prints its SHA-256 fingerprint and the LAN URLs you can open on your phone.

## Usage

1. On the desktop run `go run .`.
2. On your phone (same Wi-Fi/LAN) open `https://<desktop-ip>:8080`.
3. Accept the self-signed certificate warning.
4. Touch or drag in the top half of the screen.
5. The desktop terminal prints JSON lines like:
   ```json
   {"type":"touchmove","t":[{"id":0,"x":0.37,"y":0.51}]}
   ```

## Notes

- Coordinates `x` and `y` are normalized to `[0,1]` relative to the trackpad area.
- The data channel is configured with `ordered: false, maxRetransmits: 0` for minimum latency; lost touch events are acceptable because the next event is the current state.
- The web client automatically reconnects if the WebSocket or WebRTC connection drops, with exponential back-off (1s → 2s → 4s … up to 10s).
- HTTPS is required because WebRTC APIs need a secure browser context; plain HTTP on a LAN IP is blocked.
