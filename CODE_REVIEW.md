# Code Review — desktop_remote_mobile_companion

A focused review of the Go server + browser client, prioritizing
maintainability and bug reduction. Issues are grouped by severity.
Line numbers refer to the current tree.

---

## A. Bugs / correctness

### A1. Lost error in `loadOrGenerateCert` (shadowed `err`)
**File:** `main.go` `loadOrGenerateCert`, final lines of the generate branch.

```go
cert, err := tls.X509KeyPair(
    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
    pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
)
fp, err := certFingerprint(certFile) // <-- reuses `err`; overwrites X509KeyPair error
return cert, fp, err
```

If `tls.X509KeyPair` fails, `err` is immediately overwritten by
`certFingerprint` (which succeeds, because the file was just written),
so `main` receives a **zero `tls.Certificate` and a nil error**, then
starts a TLS server that will fail at handshake time with a confusing
message instead of a clear startup error.

**Fix:** check the X509KeyPair error before computing the fingerprint,
or rename the second variable:

```go
cert, err := tls.X509KeyPair(certPEM, keyPEM)
if err != nil {
    return tls.Certificate{}, "", fmt.Errorf("parse generated keypair: %w", err)
}
fp, err := certFingerprint(certFile)
return cert, fp, err
```

### A2. Unclamped tablet coordinates can leave the axis range
**File:** `tablet/tablet_linux.go` `ProcessEvent` (down/move branches):

```go
d.x = int32(t.X * float64(axisMax))
d.y = int32(t.Y * float64(axisMax))
```

There is no clamping. `input.Norm`/`DenormUni` exist *precisely* to clamp
to `[0,1]` and map to an axis range, but the tablet ignores them. A
pointer sample with `x`/`y` slightly outside `[0,1]` (possible during
pointer-capture drags at surface edges) produces negative or
`> axisMax` ABS values, which the kernel may reject or libinput may
mis-classify.

**Fix:** use the existing helper for consistency with the trackpad:

```go
d.x = input.DenormUni(float32(t.X), 0, axisMax)
d.y = input.DenormUni(float32(t.Y), 0, axisMax)
```

### A3. Ignored write/close errors when persisting the generated cert
**File:** `main.go` `loadOrGenerateCert`.

`pem.Encode(certOut, …)`, `certOut.Close()`, `keyOut.Close()` and
`x509.MarshalECPrivateKey` errors are all discarded. A failed key write
(for full disk, read-only cache dir, etc.) would silently leave a cert
with no usable key file, and the next run would log "key file missing"
and regenerate forever.

**Fix:** wrap in a small helper that encodes, closes, and checks errors,
or use `os.WriteFile` + `pem.EncodeToMemory`.

---

## B. Dead code & redundant abstractions

### B1. `trackpad.Device.slotID` is write-only
**File:** `trackpad/trackpad_linux.go` (fields `slotID [maxSlots]int`,
plus writes in `New`, `acquireSlot`, `releaseSlot`).

`slotID` is maintained as a "slot → browser id" reverse map but is **never
read** anywhere. The `active map[int]TouchSlot` already tracks
browser id → slot, and `releaseSlot` only needs the slot index (which the
caller already has from `d.active[id].Slot`). This is pure dead state
that must be kept consistent for no benefit.

**Fix:** delete the `slotID` field and the three lines that touch it.
`acquireSlot` simply returns `touchpad.TouchSlot{Slot: i}`; `releaseSlot`
just clears `d.slots[slot]`.

### B2. `video.Streamer.encoderOpened` and `filterBuilt` are unused
**File:** `video/video_linux.go` (lines ~74-76).

Both fields are declared with a comment saying they "avoid re-logging
one-time setup each frame", but nothing ever reads or writes them
(lazy init is guarded by `if s.filterGraph != nil` / `if s.encodeCodecContext != nil`
instead). Dead fields.

**Fix:** delete both fields and their doc comment.

### B3. `framesWritten` carries a misleading `//nolint:unused`
**File:** `video/video_linux.go` (~line 72).

The field *is* used (`atomic.AddUint64` / `atomic.LoadUint64`), so the
`nolint:unused` directive is wrong and will mask future real
unused-field warnings. Remove the directive.

### B4. Legacy `touch*` event branches are unreachable
**Files:** `trackpad/trackpad_linux.go` and `tablet/tablet_linux.go`
(`ProcessEvent` type switches).

Both devices accept `touchstart`, `touchmove`, `touchend`, `touchcancel`,
but the browser client (`static/app.js`) only ever sends `pointerdown`,
`pointermove`, `pointerup`, `pointercancel`, `buttondown`, `buttonup`
(see AGENTS.md "Pointer events vs. touch events"). The `touch*` cases
are dead defensive code that doubles the surface area of each switch.

**Fix:** drop the `touch*` cases (or, if you genuinely want belt-and-
braces robustness against a future client, keep *one* canonical mapping
in the `input` package — see B5 — instead of duplicating the table in
both devices).

### B5. Duplicated pointer-event → action mapping
**Files:** `trackpad/trackpad_linux.go`, `tablet/tablet_linux.go`.

Both devices independently translate `ev.Type` → `"down"|"move"|"up"|"button"`
with the same logic. This is a DRY violation and the two copies have
already drifted (trackpad treats `button*` as a no-op return-nil;
tablet handles them).

**Fix:** centralize in `input/event.go`:

```go
// Action classifies a browser event type. Empty string means unknown.
func Action(t string) string {
    switch t {
    case "pointerdown":               return "down"
    case "pointermove":               return "move"
    case "pointerup", "pointercancel": return "up"
    case "buttondown", "buttonup":     return "button"
    }
    return ""
}
```

Then each device does `switch input.Action(ev.Type)` once.

### B6. `Norm` + `DenormBi` round-trip is an unnecessary indirection
**File:** `input/event.go`, used only by `trackpad/trackpad_linux.go`.

The trackpad stores `slot.X = input.Norm(t.X)` (maps `[0,1]→[-1,1]`),
then emits `input.DenormBi(primary.X, 0, axisMax)` (maps `[-1,1]→[0,axisMax]`).
The composition is exactly `DenormUni(t.X, 0, axisMax)`. `Norm` and
`DenormBi` are used **only** for this trackpad X/Y path; nothing else
uses the `[-1,1]` representation.

**Fix:** store the clamped `[0,1]` value directly and emit with
`DenormUni`. Then delete `Norm` and `DenormBi` from `input/event.go`.
This also makes the trackpad and tablet use the *same* coordinate
helper (see A2), removing one more subtle inconsistency.

---

## C. Coupling & structure

### C1. Mutable package-level globals `videoEnabled` and `cli`
**File:** `main.go`.

`signalHandler` reads both the global `videoEnabled` (mutated inside
`main` after the CAP_SYS_ADMIN check) and the global `cli` struct. This
is hidden coupling: a reader of `signalHandler`'s signature cannot tell
what configuration it depends on, and the globals make the package
untestable in isolation (you cannot run two configs in the same
process).

**Fix:** define a small config struct and pass it in:

```go
type serverConfig struct {
    VideoEnabled bool
    Video        video.Config
}

func signalHandler(cfg serverConfig, pad *trackpad.Device, tab *tablet.Device) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) { ... }
}
```

`arg.MustParse` can target a local instead of the package-level `cli`
variable; pass the needed fields into `serverConfig` at startup. This
also removes the "comment explaining why videoEnabled is a package var".

### C2. `signalHandler` is too long (≈150 lines, several responsibilities)
**File:** `main.go`.

It does: WebSocket upgrade, peer-connection setup, ICE forwarding,
data-channel event routing, video-pipeline construction, the offer/answer
loop, and video lifecycle. Mixing these makes the video negotiation
especially hard to follow (the `videoEnabled && hasVideoMedia(...) &&
videoStreamer == nil` block is nested inside the `case "offer":`).

**Fix:** extract helpers:
- `handleDataChannel(dc, pad, tab, remote)`
- `maybeAddVideoTrack(pc, cfg, remote) *video.Streamer` (returns the
  streamer or nil, encapsulating the "fail open" logic and its log lines)
- `runSignalLoop(ws, pc, vs, remote)`

This shrinks `signalHandler` to a wiring function and makes the video
fallback policy independently readable/testable.

---

## D. Readability / idiomatic Go

### D1. Awkward frame-duration computation
**File:** `video/video_linux.go`, `captureLoop`:

```go
frameDuration := time.Duration(float64(time.Second) * float64(1) / float64(s.cfg.FrameRate))
```

**Fix:**

```go
frameDuration := time.Second / time.Duration(s.cfg.FrameRate)
```

Integer division of `time.Duration` is exact here and far clearer.

### D2. Deeply nested capture/encode loop
**File:** `video/video_linux.go`, `captureLoop`.

Four levels of nesting (read → send packet → receive frame → filter →
receive packet) with `continue`/`break` scattered through. Each stage
also interleaves `Unref()` bookkeeping.

**Fix:** extract three named helpers that each own one stage and its
cleanup:
`decodeOneFrame() (*astiav.Frame, bool)`, `filterFrame(in) (*astiav.Frame, bool)`,
`encodeFrame(in)`. The top loop then reads as "decode, filter, encode,
write" with a flat structure, and the `Unref` discipline lives next to
the allocation it balances.

### D3. `hasVideoMedia` is too lax
**File:** `main.go`:

```go
func hasVideoMedia(sdp string) bool { return strings.Contains(sdp, "m=video") }
```

`m=video` only ever appears at the start of an SDP line, but
`strings.Contains` would also match the substring inside an attribute
value. Not a current bug, but cheap to make precise:

```go
return strings.Contains(sdp, "\nm=video")
```
(Then also handle a leading `m=video` at offset 0 if an SDP could start
with it — it cannot, SDP starts with `v=`, so `\n` is safe.)

### D4. `boolToInt` is reinvented
**File:** `tablet/tablet_linux.go`.

Trivial, but Go has no built-in. Either keep it as a tiny package-level
helper (it already is) or inline a small `map[bool]int32`-free expression.
Not worth changing on its own; mentioned only so it isn't re-added
elsewhere. If you adopt B5's `input.Action`, you could also expose a
shared `input.BoolToInt` to avoid a second copy appearing.

### D5. `localIPs()` called twice and may emit duplicate SANs
**File:** `main.go`.

`localIPs()` is invoked once while building the certificate template and
once for the QR-code logging. It also appends *every* non-loopback
address, so an interface with two IPv4s yields duplicate-looking entries
(not a cert problem, but noisy logs). Cache the result in `main` and pass
it to both `loadOrGenerateCert` and the logging loop.

---

## E. Frontend / static assets

### E1. The two `.area` carousels are duplicated verbatim in `index.html`
**File:** `static/index.html` (~lines 175-230 and 232-280 are identical).

Every structural or styling change to a panel must be made twice, and
they already must stay in sync with `PANEL_ORDER` in `app.js`. This is the
single biggest maintainability hazard in the static assets.

**Fix (pick one):**
- Render the second area by cloning a `<template>` in `app.js` at
  startup (`const tpl = document.getElementById('area-template');
  areaBottom.replaceWith(tpl.content.cloneNode(true))`), giving it
  `id="area-bottom"`.
- Or generate both areas from a single JS function driven by
  `PANEL_ORDER`.

Either way the markup exists once.

### E2. `releaseAllPointers` fabricates a fake `PointerEvent`
**File:** `static/app.js`.

```js
sendPointerSample('pointerup', {
  clientX: rect.left, clientY: rect.top, pointerId: pid
}, info.device, info.surface);
```

It builds a synthetic object with only the three fields `sendPointerSample`
reads. This works today but is fragile (if `sendPointerSample` ever reads
`e.pressure`, `e.width`, etc., it silently gets `undefined`). Either keep
the last real `PointerEvent` per pointer in `activePointers` and replay
it, or send a dedicated `{type:"pointerup", id}` payload that doesn't
pretend to be a `PointerEvent`.

### E3. Magic index `areaEl.id === 'area-bottom' ? 2 : 0`
**File:** `static/app.js`, `initArea`.

The initial panel index is hard-coded against an element id. If a third
area is ever added this silently breaks. Pass the initial index via a
`data-initial-panel` attribute on the `.area` element and read it in
`initArea`; this also pairs naturally with E1's template approach.

### E4. `conn` is a module-level mutable singleton
**File:** `static/app.js`.

The connection state is a single global `conn` object touched by
`connect`, `closeOldConnections`, `scheduleReconnect`, the WS/PC
callbacks, and the video track handler. The `currentId` guard pattern
works but is hard to reason about. A small `class Connection` wrapping
this state (with `connect()`, `close()`, `send()`, and a single
`state` field) would localize the lifecycle and make the
"ignore stale callback" logic explicit instead of `myId !== conn.currentId`
sprinkled across a dozen callbacks. This is a refactor, not a bug fix;
defer unless you're already touching this code.

---

## F. Minor / nits

- **`video.Streamer.Stop` double-close logic** (`video/video_linux.go`): the
  `select { case <-s.stop: return; default: close(s.stop) }` is correct
  but a comment that the second call is a no-op would help; alternatively
  `sync.Once` makes the intent unambiguous:
  ```go
  var once sync.Once
  once.Do(func() { close(s.stop) })
  <-s.done
  s.freeVideoCoding()
  ```
- **`fmt.Printf("%s\n", string(msg.Data))`** in `main.go` data-channel
  handler: fine, but if event volume ever matters this is the hot path;
  a single `os.Stdout.Write` (plus a newline) avoids the `%s` reflection
  and the string copy. Premature for now — leave it.
- **`AGENTS.md` typo** ("sored" → "stored" in the Version-numbers
  paragraph) and `VERSION` file has no trailing newline handling beyond
  `TrimSpace` (fine).
- **`sw.js` / manifest.json** are kept only to evict stale service
  workers; consider deleting `sw.js` entirely once a few releases have
  passed, and shrinking `manifest.json` if PWA install is no longer a
  goal. Document the retention period so it doesn't live forever.

---

## Suggested ordering

1. **A1, A2, A3** — small, localized correctness fixes. Do first.
2. **B1, B2, B3** — trivial dead-code removal; zero risk.
3. **B5, B6** — centralize `Action` and drop `Norm`/`DenormBi`; touches
   `input`, `trackpad`, `tablet` together but each change is mechanical.
4. **B4** — drop legacy `touch*` branches once B5 lands (the single
   `Action` function makes the pruning a one-line change).
5. **C1, C2** — pass config explicitly and split `signalHandler`. Best
   done together; improves testability.
6. **E1** — de-duplicate the area markup; biggest static-asset win.
7. **D1, D2, D3, D5** — readability, low risk.
8. **E2, E3, E4, F** — as opportunity allows.
