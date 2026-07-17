# AGENTS.md — desktop_remote_mobile_companion

This file contains the context coding agents need to work on this project.

## Project overview

This project turns a mobile device into a multitouch trackpad/remote input device for a Linux desktop PC. Stage 1 is a Go program that:

1. Serves a single-page web app over HTTPS.
2. Provides WebSocket-based WebRTC signaling.
3. Receives touch events from the phone over a WebRTC data channel.
4. Prints the events to the terminal as raw JSON.

Later stages will translate those touch events into Linux input events (mouse, multitouch gestures, etc.).

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

- `main.go` — HTTPS server, self-signed certificate generation, WebSocket signaling handler, WebRTC peer connection, data-channel receiver.
- `static/index.html` — responsive trackpad UI.
- `static/app.js` — browser WebRTC client and touch-event capture.

## Build and run

```bash
# default port 8080
go run .

# custom port
go run . --port 8443
# or
go run . -p 8443
```

Build the binary:

```bash
go build .
```

The static HTML/JS assets are embedded into the binary using `//go:embed`, so the single compiled binary is self-contained.

## Configuration

Command-line flags are handled by `github.com/alexflint/go-arg`.

| Flag | Default | Description |
|---|---|---|
| `-p`, `--port` | `8080` | HTTPS listen port |

Example:

```bash
./desktop_remote_mobile_companion --port 8443
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

## WebRTC signaling flow

1. Browser opens `wss://<host>/signal`.
2. Browser creates an `RTCPeerConnection` + `RTCDataChannel('touch')`.
3. Browser sends `offer` SDP.
4. Server sets remote description, creates `answer`, sets local description, sends `answer`.
5. Both sides exchange trickle `candidate` messages as ICE candidates are discovered.
6. Once the data channel opens, the browser streams touch events to the server.
7. If the WebSocket or WebRTC connection is lost, the client automatically closes the old peer connection and creates a new one, with exponential back-off (1s → 2s → 4s … max 10s).

## Touch event format

Each browser touch event produces one JSON message sent over the data channel:

```json
{
  "type": "touchmove",
  "t": [
    {"id": 0, "x": 0.37, "y": 0.51}
  ]
}
```

- `type`: one of `touchstart`, `touchmove`, `touchend`, `touchcancel`.
- `t`: array of changed touches for that event.
- `id`: touch identifier (`Touch.identifier`).
- `x`, `y`: normalized to `[0,1]` relative to the top-half trackpad element.

The server prints the raw JSON line to stdout using `fmt.Printf`.

## Dependencies

Key Go modules:

- `github.com/pion/webrtc/v4` — WebRTC peer connection and data channel.
- `github.com/gorilla/websocket` — WebSocket signaling server.
- `github.com/alexflint/go-arg` — command-line argument parsing.

No JavaScript build step is used; `static/app.js` is served as-is.

## Development conventions

- Keep changes minimal and focused on the current stage.
- Follow existing Go formatting (`gofmt`).
- Do not commit generated files (`server.crt`, `server.key`, binary). They are `.gitignore`d.
- Static assets must remain embeddable; avoid generating files at runtime that the binary depends on.

## Testing

Manual test flow:

1. `go run .`
2. Open the printed LAN URL in a phone browser.
3. Accept the self-signed certificate.
4. Verify the status area shows “Data channel open”.
5. Touch the top half of the screen and confirm JSON lines appear in the desktop terminal.

Automated headless Chromium checks are possible with `--ignore-certificate-errors` and `--virtual-time-budget`, but WebRTC connection setup timing is flaky in headless mode; prefer manual device testing.

## Future stages

- Stage 2: translate touch events into Linux mouse/touchpad input (via `uinput` or `evdev`).
- Stage 3: multitouch gestures (two-finger scroll, pinch, etc.).
- Stage 4: optional pairing/authentication, connection reliability, and UI polish.
