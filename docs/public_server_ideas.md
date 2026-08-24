# Public Server Hosting Ideas (design discussion, no implementation yet)

Status: **discussion only** — nothing in this document has been implemented.
This documents two architectural options explored for serving the web client
(`server/static/index.html` + `app.js`) from a publicly-hosted static web
server (GitHub Pages style) or public relay, while the input-receiving
desktop instance stays on the local LAN.

Goals motivating the discussion:

- Make the web app a proper installable PWA from a trusted HTTPS origin
  (no self-signed-cert warning on every phone visit).
- Remove the per-phone cert-acceptance dance if possible.
- Possibly get NAT traversal / off-LAN operation for free.
- Keep the existing direct same-origin LAN mode working (it has the best
  latency and no external dependency).

---

## Background: how the current architecture couples TLS to the LAN

Today the phone browser **connects into** the desktop:

```
Phone browser -- HTTPS page load --> desktop server (self-signed cert)
Phone browser -- wss://<lan-ip>:8080/signal -/-> desktop server
Phone browser <-- WebRTC data channel + video ----> desktop server
```

Two different cryptographies are at play, and it matters to keep them apart:

1. **The signaling channel** — WebRTC deliberately does not specify this;
   it is "whatever works" for exchanging SDP blobs and ICE candidates. It
   only has to exist, and ideally be tamper-evident.
2. **WebRTC itself** — once connected, the data channel and media are
   encrypted end-to-end with DTLS, keyed by self-signed key pairs each peer
   generates per connection. The integrity anchor is the certificate
   fingerprint embedded in the SDP: if the signaling channel delivers the
   SDP untampered, the resulting WebRTC connection cannot be MITM'd. **WebRTC
   never requires either peer to present a publicly-trusted TLS
   certificate** — the DTLS fingerprints are public key material, not certs
   that must chain to a root CA.

The LAN desktop only needs a TLS cert at all because browsers demand TLS on
*inbound* connections (page loads and WebSocket handshakes). All the
trouble dissolves if the desktop dials *outbound* instead of being a server
(see Option B).

---

## Option A: static-hosted page + page connects into the LAN server

Keep the desktop as the listening server; host only the HTML/JS on a public
static host. The page gets the desktop's LAN address (manual entry, QR code,
…), then opens `wss://<lan-ip>:8080/signal` cross-origin.

### What already works in our favor

- `server/server.go` already sets `CheckOrigin: func(r *http.Request) bool { return true }`
  on the Gorilla upgrader, so a WebSocket whose `Origin` is a public domain
  is accepted.
- An HTTPS-hosted page is a *better* secure context than the current
  self-signed LAN page — no WebKit quirks about untrusted certs gating
  WebRTC APIs.
- The signaling traffic is tiny; the heavy paths (data channel events,
  H264 RTP video) ride host ICE candidates entirely on the LAN and never
  touch the public host.
- ICE/WebRTC do not care about the page origin at all.

### The obstacles

1. **TLS on the LAN (the main friction).** The page's `fetch` and WebSocket
   still need to trust the LAN server's self-signed cert; browsers fail
   `wss://` *silently* on an untrusted cert. Workaround: visit
   `https://<lan-ip>:8080` once, accept the cert warning — the exception
   also applies to the WebSocket. Polished alternative (Plex / fritz.box
   pattern): real domain + wildcard cert with DNS rebinding, e.g.
   `<ip>.inara.example.com` → `192.168.1.5`, giving publicly-valid certs on
   the LAN. Requires Let's Encrypt DNS-challenge automation, shipping key
   material, and some routers' DNS-rebinding protection blocks it. Probably
   overkill for now.

2. **Chrome Private Network Access (PNA).** A *public* page making requests
   to a *private* IP triggers PNA preflights: Chrome sends OPTIONS with
   `Access-Control-Request-Private-Network: true` on fetch and, in recent
   versions, on WebSocket handshakes. The LAN server must reply with:
   - `Access-Control-Allow-Private-Network: true`
   - `Access-Control-Allow-Origin: https://<your-hosted-origin>` —
     **pin this**, otherwise any website could try to drive your input
     devices.
   - `Access-Control-Allow-Credentials: true` if cookies are used.

3. **Auth/cookies don't survive cross-site.** The passcode flow
   (`--passcode`, `server/auth.go`) sets an HttpOnly cookie on the LAN
   origin; served from a public page that cookie is *third-party*: needs
   `SameSite=None; Secure`, and Chrome's third-party cookie blocking may
   still kill it. Cleaner fix: have `POST /auth` return the session token
   as JSON and pass it to `/signal` as a WebSocket URL query parameter or
   `Sec-WebSocket-Protocol` subprotocol value (browsers cannot set
   arbitrary headers on WebSocket). Same security properties, no cookie
   fragility.

4. **Discovery: how does the page find the LAN server?** A static page
   cannot know the LAN IP. Options, in effort order: manual entry
   (persist in `localStorage`); QR code shown by the server/GUI encoding
   `https://hosted.app/?server=192.168.1.5:8080`; mDNS hostname
   (`inara.local` — works on Apple devices, flaky elsewhere); local DNS.
   Manual + QR is the pragmatic answer.

5. **Version injection.** The server rewrites the embedded HTML with the
   embedded `server/VERSION` string at serve time; a statically-hosted copy
   gets the un-injected placeholder. Fix: expose `/version` as JSON (or
   piggyback on the `/signal` handshake) and let the client populate the
   label dynamically.

### Minimal implementation shape (Option A)

1. Client: read `?server=host:port` from the URL (default `location.host`),
   persist in `localStorage`, use it for `/auth`, `/version`, and the
   `wss://…/signal` URL.
2. Server: CORS middleware handling PNA preflights, with the allowed origin
   pinned at build/config time.
3. Auth: accept the session token via WS query param (or subprotocol) in
   addition to the cookie.
4. ICE, data channel routing, video pipeline, tablet remapping: untouched.

Verdict: feasible and moderately cheap. The self-signed cert acceptance
dance on first connect remains.

---

## Option B: public rendezvous/relay, "code exchange" style
(file.pizza / PairDrop / magic-wormhole model)

Those "enter a 4-word code, no certs anywhere" sites work by exploiting the
two-layer separation above: the code is a **rendezvous key**, not a
credential, and the middleman does TLS. Architecture flips from
"phone connects into desktop" to "everyone dials out":

```
phone browser --(public static HTTPS page)--> rendezvous (public, valid cert)
       │                                            ▲
       └──────── WebRTC data channel ──────────────┘
               (DTLS, P2P/relay after handshake)
desktop inara binary --(outbound WSS dial, code)--> rendezvous
```

- The desktop binary embeds a tiny WebSocket *client* to a rendezvous
  service, announces a code/room, and answers offers. **No listen port, no
  cert, no cert-acceptance dance, and off-LAN/NAT operation comes free**
  (direct host candidates on same LAN; TURN relay fallback for hard NATs).
- The static page does the same from the browser.
- The rendezvous server is a dumb ~100-line relay: room membership +
  forward `offer`/`answer`/`candidate` messages.

### How existing projects realize the code

- **PairDrop / Snapdrop**: `wss://` to a public relay; the code (or LAN
  auto-discovery via the relay) maps peers; relay forwards signaling.
  PairDrop additionally performs an ephemeral ECDH key exchange *with the
  code as context* so an evil relay cannot MITM.
- **file.pizza**: public WebTorrent trackers over WebSocket as the broker;
  the magnet hash is the code.
- **PeerJS**: cloud-hosted broker; the peer ID doubles as the code.
- **magic-wormhole / croc**: the reference implementation — the short code
  feeds a **PAKE (SPAKE2)**: it both rendezvouses *and* proves both sides
  know the same code, making even a malicious relay incapable of MITM.
  This properly satisfies the "tamper-evident signaling" requirement.

### Security considerations specific to inara

Unlike a file transfer, a successful connection here can **drive the
desktop's input devices** (the entire purpose of the app). Therefore:

- **The code becomes the auth secret**, replacing/complementing
  `--passcode`. It needs meaningful entropy (wormhole-style
  "7-saddened-dimly" word pairs give ~16–20 bits amplified by PAKE;
  random multi-word slugs give more) plus per-room rate-limiting against
  guessing/probing.
- Strongly consider a *pairing confirmation on the desktop side*
  ("phone at <browser> wants to control input — allow? [y/N]") at least
  for the first connection per room/code, since remote control is a much
  higher-stakes handshake than sending a photo.
- The relay sees only ciphertext + SDP (never pointer data), provided the
  code-authenticated key exchange (PairDrop ECDH over code, or full SPAKE2)
  protects the DTLS fingerprints.
- Data-channel traffic remains end-to-end DTLS-encrypted regardless of the
  relay.

### Trade-offs vs Option A

Gains:
- Zero certs on the LAN, zero cert warnings, zero PNA/CORS work.
- Works across NAT / off-LAN (great for showing it off away from home).
- Strictly equal-or-better crypto: verified DTLS fingerprints + PAKE vs an
  unverified self-signed cert exception nobody actually checks the
  fingerprint of.

Costs:
- A public relay service to write, host, and trust (metadata: room codes,
  connection times, IPs of both peers).
- No longer fully self-contained; a working internet path becomes a hard
  requirement for the new mode.
- Code-guessing/abuse surface on the public relay (rate limits, room TTL).
- TURN server needed for symmetric-NAT scenarios if off-LAN equality
  matters; without TURN those connections just fail.

### Suggested sequencing if Option B is pursued

1. Extract the signaling role in the desktop binary into a "dialer" mode
   (outbound WSS to the relay) alongside today's "server" mode; keep the
   direct same-origin mode as the no-cloud, lowest-latency path.
2. Stand up the minimal relay (rooms keyed by code; forward everything).
3. Add code-authenticated fingerprint verification — PairDrop-style
   ephemeral ECDH keyed by the code, or full SPAKE2 for the magic-wormhole
   level.
4. Add a desktop-side pairing confirmation UX.
5. Optionally add TURN and cross-NAT behavior as phase 2.

---

## Comparison summary

| Concern | Option A (static page + LAN connect-in) | Option B (public rendezvous/code) |
|---|---|---|
| Desktop needs TLS cert | yes (self-signed; user accepts once, or DNS-rebind wildcards) | no |
| Cert dance on phone | once per device/LAN IP | none |
| Chrome PNA/CORS changes | required | none |
| Cross-site auth | token-in-WS-query rework | n/a (code is the key) |
| Server discovery | manual/QR/mDNS | code → relay room (trivial UX) |
| Works off-LAN / NAT | no | yes (with TURN fallback) |
| External dependency | none beyond static host | relay service (small) |
| Relay sees data | n/a (no relay) | SDP + ciphertext only |
| Latency on LAN | best | peer-to-peer on same LAN ≈ same |
| New server-side code | CORS/PNA middleware, `/version` JSON | relay + dialer mode + PAKE |

## Open questions for the next session

- Is off-LAN operation actually desired (drives whether B's relay is a
  requirement or optional)?
- Keep passcode + local mode as-is forever, or migrate everything to codes?
- Acceptable entropy/typing burden for the code on a phone keyboard?
- Desktop confirm prompt: CLI only, or does the GUI need an "allow" dialog?
- If Option A: which static origin gets pinned in Access-Control-Allow-Origin,
  and is a custom domain wanted?
