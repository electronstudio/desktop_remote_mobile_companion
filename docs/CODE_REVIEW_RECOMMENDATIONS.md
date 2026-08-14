# Code Review Recommendations

This document contains concrete, maintainability-focused recommendations for the `inara` codebase. The goal is to reduce bugs, simplify logic, and make the code easier to change without premature optimization.

---

## 1. Introduce an `EventProcessor` interface and route via a map

**Problem**  
`../main.go`'s data-channel callback hard-codes routing between the trackpad and tablet using string literals and concrete types:

```go
switch ev.Device {
case "tablet":
    tabletDev.ProcessEvent(ev)
case "trackpad":
    pad.ProcessEvent(ev)
}
```

This couples the signaling layer to the two concrete device packages, violates the Open/Closed Principle, and forces future devices to edit `../main.go`.

**Recommended Change**  
Define a small interface that both devices already satisfy, then build a route table:

```go
// input/processor.go
package input

type EventProcessor interface {
    ProcessEvent(Event) error
    Close() error
}

// main.go (inside a Server struct or similar)
processors := map[string]input.EventProcessor{
    "trackpad": pad,
    "tablet":   tabletDev,
}
```

Then the data-channel handler becomes:

```go
proc, ok := s.processors[ev.Device]
if !ok {
    log.Printf("unknown device %q", ev.Device)
    return
}
if err := proc.ProcessEvent(ev); err != nil {
    log.Printf("%s event error: %v", ev.Device, err)
}
```

**Files / Functions**  
- `../input/event.go` (add interface) or new `input/processor.go`
- `../main.go`: `signalHandler` data-channel callback

---

## 2. Replace package-level mutable state with a `Server` struct

**Problem**  
`../main.go` currently relies on package-level variables:

```go
var videoEnabled bool
var upgrader = websocket.Upgrader{...}
var cli struct{...}
```

`signalHandler` indirectly depends on both `videoEnabled` and `cli` (for video config). This makes the handler impossible to unit-test without mutating global state and creates tight coupling between CLI parsing, capability checks, and request handling.

**Recommended Change**  
Create a `Server` struct that holds all runtime dependencies and pass it explicitly:

```go
type Server struct {
    Port        int
    VideoConfig video.Config
    VideoEnabled bool
    Trackpad    input.EventProcessor
    Tablet      input.EventProcessor
    StaticFS    http.FileSystem
    IndexHTML   []byte
    Cert        tls.Certificate
}

func (s *Server) signalHandler(w http.ResponseWriter, r *http.Request) { ... }
```

`main` becomes responsible only for wiring: parsing flags, checking capabilities, creating devices, and constructing `Server`. HTTP handlers become methods on `Server`.

**Files / Functions**  
- `../main.go`: `main`, `signalHandler`, package-level `var` declarations

---

## 3. Move WebRTC signaling logic out of `../main.go`

**Problem**  
`signalHandler` is ~190 lines and mixes WebSocket framing, WebRTC state machine, video streamer lifecycle, data-channel routing, and ICE candidate relaying. `../main.go` is therefore doing TLS, HTTP serving, static asset serving, certificate management, device creation, *and* WebRTC signaling.

**Recommended Change**  
Extract a `signaling` package or at minimum a `signal.Session` type:

```go
package signaling

type Session struct {
    WS           *websocket.Conn
    PC           *webrtc.PeerConnection
    VideoEnabled bool
    VideoConfig  video.Config
    ProcessEvent func(input.Event)
}

func (s *Session) Run() error
func (s *Session) handleOffer(msg signalMsg) error
func (s *Session) handleCandidate(msg signalMsg) error
```

This keeps `../main.go` as pure wiring and makes the signaling flow testable in isolation.

**Files / Functions**  
- `../main.go`: `signalHandler`, `signalMsg`
- New file/package: `signaling/session.go` (or `internal/signaling/`)

---

## 4. Fix and simplify the certificate-loading logic

**Problem**  
`loadOrGenerateCert` in `../main.go` uses nested short-variable declarations that shadow `err` across multiple blocks:

```go
cert, err := tls.LoadX509KeyPair(...)
fp, err := certFingerprint(certFile)
return cert, fp, err
```

The scoping is hard to follow and the function mixes loading, generation, and PEM encoding. It is also not easily testable because it touches the filesystem and generates random keys.

**Recommended Change**  
Split into small, pure-ish helpers:

```go
func loadExistingCert(certFile, keyFile string) (tls.Certificate, string, error)
func generateCert(certFile, keyFile string) (tls.Certificate, string, error)
func writeKey(path string, key *ecdsa.PrivateKey) error
func fingerprintFromFile(path string) (string, error)
```

Use named return values sparingly or avoid them where `:=` shadows. Return `(tls.Certificate, string, error)` from each helper so the caller is explicit. Consider making the cert generation inject `rand.Reader` for tests.

**Files / Functions**  
- `../main.go`: `loadOrGenerateCert`, `certFingerprint`

---

## 5. Use typed constants for event and device names

**Problem**  
Event type strings (`"pointerdown"`, `"pointermove"`, `"buttondown"`) and device names (`"trackpad"`, `"tablet"`) are repeated in Go and JavaScript. They are easy to misspell and cannot be checked by the compiler.

**Recommended Change**  
Add constants in `../input/event.go`:

```go
const (
    DeviceTrackpad = "trackpad"
    DeviceTablet   = "tablet"

    EventPointerDown   = "pointerdown"
    EventPointerMove   = "pointermove"
    EventPointerUp     = "pointerup"
    EventPointerCancel = "pointercancel"
    EventButtonDown    = "buttondown"
    EventButtonUp      = "buttonup"
)
```

Use them in `trackpad.ProcessEvent`, `tablet.ProcessEvent`, and the route map in `../main.go`. For the JavaScript side, define a matching block at the top of `../static/app.js` and reference it everywhere:

```js
const EVENT = {
  POINTER_DOWN: 'pointerdown',
  POINTER_MOVE: 'pointermove',
  // ...
};
const DEVICE = { TRACKPAD: 'trackpad', TABLET: 'tablet' };
```

**Files / Functions**  
- `../input/event.go`
- `../trackpad/trackpad_linux.go`: `ProcessEvent`
- `../tablet/tablet_linux.go`: `ProcessEvent`
- `../main.go`: route table
- `../static/app.js`: send functions

---

## 6. Simplify and clarify `trackpad.ProcessEvent`

**Problem**  
The function first maps event names to an intermediate `action` string (`"down"`, `"move"`, `"up"`), then switches on `action`. It also maintains three parallel slot-tracking structures:

```go
active map[int]touchpad.TouchSlot
slots  [maxSlots]bool
slotID [maxSlots]int
```

`slots` and `slotID` are derivable from `active`, yet they are kept in sync manually. The function is ~90 lines long and mixes event routing, slot allocation, event emission, and cleanup.

**Recommended Change**  
Remove the intermediate `action` variable and handle event types directly (or use a helper that returns a lifecycle enum). Replace the parallel arrays with a single free-slot list derived from the map:

```go
func (d *Device) firstFreeSlot() (int, bool) {
    used := make(map[int]struct{}, len(d.active))
    for _, slot := range d.active {
        used[slot.Slot] = struct{}{}
    }
    for i := 0; i < maxSlots; i++ {
        if _, ok := used[i]; !ok {
            return i, true
        }
    }
    return 0, false
}
```

Alternatively, keep a `[]int` free list if slot churn is high; for 10 slots either approach is fine. The important part is storing slot state in one place.

Split `ProcessEvent` into helpers:

```go
func (d *Device) pointerDown(t input.Touch) error
func (d *Device) pointerMove(t input.Touch) error
func (d *Device) pointerUp(id int) error
func (d *Device) emitFrame(slots []touchpad.TouchSlot) error
```

**Files / Functions**  
- `../trackpad/trackpad_linux.go`: `ProcessEvent`, `acquireSlot`, `releaseSlot`

---

## 7. Collapse duplicated cases in `tablet.ProcessEvent` and fix proximity-out

**Problem**  
`tablet.ProcessEvent` duplicates identical code for `touchstart`/`pointerdown`, `touchmove`/`pointermove`, and `touchend`/`touchcancel`/`pointerup`/`pointercancel`. The duplication makes it easy for the four cases to drift.

More importantly, `proximityOut` does not release `BTN_TOUCH` or barrel buttons. If the tool leaves proximity while a tip or barrel button is pressed, those keys stay pressed:

```go
func (d *Device) proximityOut() {
    d.vd.ReleaseButton(linux.BTN_TOOL_PEN)
    d.vd.SendMiscEvent(linux.MSC_SERIAL, 0)
    d.inRange = false
    d.vd.SyncReport()
}
```

**Recommended Change**  
Group event types by behavior:

```go
switch ev.Type {
case input.EventPointerDown, input.EventPointerMove:
    // update x,y and emit hover / proximity-in
...
case input.EventPointerUp, input.EventPointerCancel:
    if d.inRange {
        d.proximityOut()
    }
}
```

Fix `proximityOut` to release all pressed buttons before the tool leaves:

```go
func (d *Device) proximityOut() {
    d.releaseButtons() // BTN_TOUCH, BTN_STYLUS, BTN_STYLUS2
    d.vd.ReleaseButton(linux.BTN_TOOL_PEN)
    d.vd.SendMiscEvent(linux.MSC_SERIAL, 0)
    d.inRange = false
    d.vd.SyncReport()
}
```

Track which buttons are currently pressed (a small set) so `releaseButtons` is accurate.

**Files / Functions**  
- `../tablet/tablet_linux.go`: `ProcessEvent`, `proximityOut`

---

## 8. Remove or re-evaluate the `input.Norm` / `DenormBi` helpers

**Problem**  
`input.Norm` converts `[0,1]` to `[-1,1]`, and `DenormBi` converts back to `[min,max]`. This round-trip is unnecessary for absolute tablet coordinates, and `tablet_linux.go` bypasses it entirely with `int32(t.X * float64(axisMax))`. The float32 conversions also silently reduce precision.

**Recommended Change**  
For absolute devices, add a single direct helper:

```go
// input/event.go
func Denorm(v float64, min, max int32) int32 {
    if v < 0 { v = 0 }
    if v > 1 { v = 1 }
    return min + int32(v*float64(max-min))
}
```

Use it in both `trackpad_linux.go` and `tablet_linux.go`. Remove `Norm` and `DenormBi` unless they are genuinely needed by the underlying virtual-device library. If they are, keep them but make the precision trade-off explicit in a comment.

**Files / Functions**  
- `../input/event.go`: `Norm`, `DenormBi`, `DenormUni`
- `../trackpad/trackpad_linux.go`: coordinate conversion
- `../tablet/tablet_linux.go`: coordinate conversion

---

## 9. Replace the SDP substring check with proper parsing

**Problem**  
`hasVideoMedia` checks `strings.Contains(sdp, "m=video")`, which is fragile and will match inside unrelated SDP attributes or comments.

```go
func hasVideoMedia(sdp string) bool {
    return strings.Contains(sdp, "m=video")
}
```

**Recommended Change**  
Use Pion's `sdp` package or `pion/webrtc`'s `TrackDetails` APIs. A minimal improvement is to parse the offer SDP before checking:

```go
import "github.com/pion/sdp/v3"

func offerHasVideo(offer string) bool {
    parsed := &sdp.SessionDescription{}
    if err := parsed.Unmarshal([]byte(offer)); err != nil {
        return false
    }
    for _, media := range parsed.MediaDescriptions {
        if media.MediaName.Media == "video" {
            return true
        }
    }
    return false
}
```

Even better: do not parse SDP at all. Instead, add the transceiver on the server side only when video is enabled before creating the answer, or inspect `pc.GetTransceivers()` after `SetRemoteDescription`.

**Files / Functions**  
- `../main.go`: `hasVideoMedia`

---

## 10. Split the `video.Streamer` mega-struct

**Problem**  
`video.Streamer` holds ~20 fields covering capture, filtering, encoding, and WebRTC track state. It is difficult to reason about which fields belong to which pipeline stage and which must be initialized/cleaned up together.

**Recommended Change**  
Group the FFmpeg objects into stage-specific structs:

```go
type Streamer struct {
    cfg   Config
    Track *webrtc.TrackLocalStaticSample
    stop  chan struct{}
    done  chan struct{}
    pts   int64

    capture  captureStage
    filter   filterStage
    encoder  encoderStage
}

type captureStage struct {
    ctx    *astiav.FormatContext
    codec  *astiav.CodecContext
    packet *astiav.Packet
    frame  *astiav.Frame
    stream *astiav.Stream
}
```

Each stage owns its own `Close()` method. The top-level `Stop()` becomes a sequence of `s.encoder.Close(); s.filter.Close(); s.capture.Close()`.

**Files / Functions**  
- `../video/video_linux.go`: `Streamer`, `freeVideoCoding`

---

## 11. Flatten the deeply nested `captureLoop`

**Problem**  
`captureLoop` contains four nested loops plus multiple `select` statements. The control flow (read → send packet → receive frame → filter → encode → receive packet → write sample) is hard to follow, and errors are handled inconsistently (some `return`, some `continue`, some `break`).

**Recommended Change**  
Extract each major step into a method that returns a sentinel error or boolean:

```go
func (s *Streamer) readPacket() error       // true when EOF
func (s *Streamer) decodeFrame() error      // ErrAgain, EOF, or fatal
func (s *Streamer) pushFilteredFrame() error
func (s *Streamer) encodeAndWrite() error
```

Then `captureLoop` reads like a pipeline:

```go
for {
    if s.stopped() { return }

    if err := s.readPacket(); errors.Is(err, astiav.ErrEof) { return }
    if err := s.decodeFrame(); err != nil { continue }
    if err := s.pushFilteredFrame(); err != nil { return }
    if err := s.encodeAndWrite(); err != nil { return }
}
```

This makes retry-vs-fatal decisions explicit and testable.

**Files / Functions**  
- `../video/video_linux.go`: `captureLoop`

---

## 12. Validate `video.Config` in one place

**Problem**  
`video.New` silently mutates its input config with defaults:

```go
if cfg.FrameRate <= 0 { cfg.FrameRate = 30 }
if cfg.QP <= 0 { cfg.QP = 24 }
if cfg.LowPower != 0 && cfg.LowPower != 1 { cfg.LowPower = 1 }
```

This is surprising for a caller that passed a config, and the defaults are duplicated with the CLI defaults in `../main.go`.

**Recommended Change**  
Add a `Config.Validate()` method that returns an error for invalid values and applies defaults only when documented:

```go
func (c *Config) Validate() error {
    if c.FrameRate <= 0 { c.FrameRate = 30 }
    if c.QP <= 0 || c.QP > 52 { c.QP = 24 }
    if c.LowPower != 0 && c.LowPower != 1 { return fmt.Errorf("low_power must be 0 or 1") }
    if c.CardPath == "" { /* auto-detect */ }
    return nil
}
```

Even better, set defaults in the CLI layer and treat zero values as errors in the library layer, so `video` does not silently change its input.

**Files / Functions**  
- `../video/video_linux.go`: `New`
- `../main.go`: CLI defaults

---

## 13. Refactor `../static/app.js` into cohesive modules

**Problem**  
The entire frontend is one ~650-line IIFE. It handles:

- Connection state and WebRTC/WebSocket lifecycle
- Pointer event marshalling
- Carousel/pan gesture logic
- Splitter / layout management
- Service-worker cleanup
- UI logging

This makes it hard to locate bugs and encourages duplication (e.g., `sendPointerSample` and `sendButtonEvent` both check the channel and call `JSON.stringify`).

**Recommended Change**  
Keep the file as plain JS (no build step required), but split it into clearly separated sections or files:

```
static/
  app.js          // entry point: wires everything together
  connection.js   // WebSocket + WebRTC lifecycle
  carousel.js     // panel panning / snapping
  input.js        // pointer/button event capture and send
  layout.js       // splitter / orientation handling
  log.js          // multi-area logger
```

If multiple files are undesirable, at least group the IIFE into local objects:

```js
const Connection = { ws: null, pc: null, ... };
const Carousel = { state: {}, ... };
const Input = { activePointers: new Map(), ... };
```

**Files / Functions**  
- `../static/app.js`: entire file

---

## 14. Deduplicate outgoing message helpers in the frontend

**Problem**  
`sendPointerSample` and `sendButtonEvent` repeat the channel check, `JSON.stringify`, and try/catch:

```js
function sendPointerSample(type, e, device, surface) {
  if (!conn.channel || conn.channel.readyState !== 'open') return;
  // ... build payload ...
  try { conn.channel.send(JSON.stringify(payload)); } catch (err) { console.error(...); }
}
```

**Recommended Change**  
Introduce a single `sendEvent(payload)` helper:

```js
function sendEvent(payload) {
  const ch = conn.channel;
  if (!ch || ch.readyState !== 'open') return false;
  try {
    ch.send(JSON.stringify(payload));
    return true;
  } catch (err) {
    console.error('send failed', err);
    return false;
  }
}

function sendPointerSample(type, e, device, surface) {
  const rect = surface.getBoundingClientRect();
  sendEvent({ device, type, t: [{ id: e.pointerId, x: ..., y: ... }] });
}
```

**Files / Functions**  
- `../static/app.js`: `sendPointerSample`, `sendButtonEvent`

---

## 15. Simplify the carousel pan math and clarify constants

**Problem**  
The carousel code uses the magic value `33.333` repeatedly, computes pan transforms inline, and mutates `style.order` in several places. The relationship between the three visible panels (`current`, `prev`, `next`) and the translate offset is hard to verify.

**Recommended Change**  
Centralize panel layout constants and derive offsets from them:

```js
const PANEL_COUNT = 3;
const PANEL_PERCENT = 100 / PANEL_COUNT; // 33.333...

function setTranslate(percent) {
  carousel.style.transform = `translateX(${percent.toFixed(3)}%)`;
}
```

Consider using CSS classes (e.g., `.panel-current`, `.panel-prev`, `.panel-next`) to set `order`, so the logic does not touch inline styles in multiple functions.

**Files / Functions**  
- `../static/app.js`: `arrangePanels`, `preparePan`, `setPanTransform`, `endPan`

---

## 16. Avoid duplicate HTML for top and bottom areas

**Problem**  
`../static/index.html` contains two nearly identical copies of the area/carousel markup. Keeping them in sync is error-prone.

**Recommended Change**  
Because there is no JS build step, use a tiny runtime duplication is acceptable, but consider generating the second area from JS:

```js
function cloneArea(id) {
  const template = document.getElementById('area-top');
  const clone = template.cloneNode(true);
  clone.id = id;
  template.parentNode.insertBefore(clone, template.nextSibling);
}
```

Alternatively, keep the HTML duplication but add an HTML-only comment warning that changes must be mirrored. The cleanest long-term fix is a small build step or a Go template for `index.html`, but that is a larger change.

**Files / Functions**  
- `../static/index.html`
- `../static/app.js`: `initArea`

---

## 17. Add unit tests for event routing and device state machines

**Problem**  
Only `trackpad` has tests, and they are manual (`MANUAL_INPUT_TEST=1`). `tablet`, `input`, and `main` have no automated tests. The slot-tracking and button-state logic is exactly the kind of state machine that benefits from fast unit tests.

**Recommended Change**  
Add interfaces/abstraction layers so tests can run without real `uinput`:

```go
// In trackpad/tablet packages
type uinputBackend interface {
    Send(uint16, uint16, int32) error
    MultiTouch([]touchpad.TouchSlot) error
    // ...
}
```

Then tests can inject a fake backend that records calls:

```go
type recorder struct{ calls []inputCall }

func TestTrackpadSingleFingerEmitsAbsXY(t *testing.T) { ... }
func TestTabletProximityOutReleasesButtons(t *testing.T) { ... }
```

**Files / Functions**  
- `../trackpad/trackpad_test.go`
- New: `tablet/tablet_test.go`
- `../input/event.go`

---

## 18. Remove or justify the Windows stub TODO comments

**Problem**  
`trackpad_windows.go` and `tablet_windows.go` contain large, stale TODO comments describing a future Windows implementation. They add noise and are not actionable in their current form.

**Recommended Change**  
Move design notes to `improvements.md` or a dedicated `docs/windows.md` file. Keep the stub files minimal:

```go
//go:build windows
package trackpad

import "github.com/.../input"

type Device struct{}
func New() (*Device, error) { return &Device{}, nil }
func (d *Device) Close() error { return nil }
func (d *Device) ProcessEvent(ev input.Event) error { return nil }
```

This keeps the build working without misleading future readers into thinking the implementation notes are part of the active codebase.

**Files / Functions**  
- `../trackpad/trackpad_windows.go`
- `../tablet/tablet_windows.go`

---

## 19. Use a structured logger and consistent log prefixes

**Problem**  
The code mixes `log.Printf` with `fmt.Printf` (for raw JSON events) and inconsistent prefixes (`video:`, `certificate:`, no prefix). This makes log aggregation and filtering harder.

**Recommended Change**  
Standardize on `log.Printf` with a component prefix everywhere, or inject a small `*log.Logger` per package. For example:

```go
var log = log.New(os.Stderr, "[tablet] ", log.LstdFlags)
```

Avoid `fmt.Printf` for server output; use `log.Printf` and let the terminal log display raw event data under a `[event]` prefix if desired.

**Files / Functions**  
- `../main.go`: `fmt.Printf` in data-channel callback
- `../video/video_linux.go`: all log lines
- `../tablet/tablet_linux.go`, `../trackpad/trackpad_linux.go`

---

## 20. Document the `//nolint:unused` annotation on `framesWritten`

**Problem**  
`video.Streamer.framesWritten` carries a `//nolint:unused` comment, but the field is clearly used via `atomic.AddUint64` and `atomic.LoadUint64`. The annotation suggests a linter confusion (possibly because the value returned by `atomic.AddUint64` is assigned back to `n` but the field itself is only read inside the atomic calls).

**Recommended Change**  
Either remove the lint annotation if it is no longer needed, or add a short comment explaining why it is required. If the linter is misidentifying the field, prefer a targeted `//nolint:staticcheck` or similar rather than a generic `unused` suppression.

**Files / Functions**  
- `../video/video_linux.go`: `framesWritten`

---

## Summary of Priority

| Priority | Recommendation | Main Benefit |
|----------|----------------|--------------|
| High | 1, 2, 3 | Decouple signaling from device handling; enables testing |
| High | 7 | Fixes a real button-stuck bug in tablet mode |
| High | 13, 14 | Makes the frontend maintainable |
| Medium | 4, 5, 6, 8 | Reduces duplication and magic values in Go |
| Medium | 9, 10, 11, 12 | Hardens video pipeline robustness and clarity |
| Medium | 15, 16 | Improves frontend readability |
| Low | 17, 18, 19, 20 | Testing, portability, logging polish |

Most of these changes can be applied incrementally; the highest-value first step is to introduce the `EventProcessor` interface and a `Server` struct, because almost every other improvement in `../main.go` becomes easier once those abstractions exist.
