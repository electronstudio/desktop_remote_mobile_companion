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

- Multi-client fan-out: a single shared capture pipeline distributing H264
  samples to N peer connections (kmsgrab is exclusive today, so only one phone
  at a time gets video?).