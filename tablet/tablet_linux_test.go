//go:build linux

package tablet

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
	virtual_device "github.com/jbdemonte/virtual-device"
	"github.com/jbdemonte/virtual-device/linux"
	"github.com/jbdemonte/virtual-device/sdl"
)

// rec is a single recorded virtual-device call.
type rec struct {
	kind   string // "send" | "abs" | "msc" | "press" | "release" | "sync"
	evType uint16
	code   uint16 // EV_KEY code (for send) or button (press/release)
	axis   linux.AbsoluteAxis
	msc    linux.MiscEvent
	btn    linux.Button
	value  int32
}

// fakeVD is a no-op virtual_device.VirtualDevice that records the calls the
// tablet.device makes (Send, SendAbsoluteEvent, SendMiscEvent, PressButton,
// ReleaseButton, SyncReport), so the emitted evdev event sequence can be
// asserted without a real /dev/uinput device. The real backend dispatches
// events onto an internal channel, so concurrent writes are safe; fakeVD
// guards its recording slice with a mutex so the same holds for tests.
type fakeVD struct {
	mu     sync.Mutex
	events []rec
}

func (f *fakeVD) WithPath(string) virtual_device.VirtualDevice            { return f }
func (f *fakeVD) WithMode(os.FileMode) virtual_device.VirtualDevice       { return f }
func (f *fakeVD) WithQueueLen(int) virtual_device.VirtualDevice           { return f }
func (f *fakeVD) WithBusType(linux.BusType) virtual_device.VirtualDevice  { return f }
func (f *fakeVD) WithVendor(sdl.Vendor) virtual_device.VirtualDevice      { return f }
func (f *fakeVD) WithProduct(sdl.Product) virtual_device.VirtualDevice    { return f }
func (f *fakeVD) WithVersion(uint16) virtual_device.VirtualDevice         { return f }
func (f *fakeVD) WithName(string) virtual_device.VirtualDevice            { return f }
func (f *fakeVD) WithKeys([]linux.Key) virtual_device.VirtualDevice       { return f }
func (f *fakeVD) WithButtons([]linux.Button) virtual_device.VirtualDevice { return f }
func (f *fakeVD) WithAbsAxes([]virtual_device.AbsAxis) virtual_device.VirtualDevice {
	return f
}
func (f *fakeVD) WithRelAxes([]linux.RelativeAxis) virtual_device.VirtualDevice { return f }
func (f *fakeVD) WithRepeat(int32, int32) virtual_device.VirtualDevice          { return f }
func (f *fakeVD) WithLEDs([]linux.Led) virtual_device.VirtualDevice             { return f }
func (f *fakeVD) WithProperties([]linux.InputProp) virtual_device.VirtualDevice {
	return f
}
func (f *fakeVD) WithMiscEvents([]linux.MiscEvent) virtual_device.VirtualDevice { return f }

func (f *fakeVD) Register() error   { return nil }
func (f *fakeVD) Unregister() error { return nil }

func (f *fakeVD) Send(evType, code uint16, value int32) {
	f.mu.Lock()
	f.events = append(f.events, rec{kind: "send", evType: evType, code: code, value: value})
	f.mu.Unlock()
}
func (f *fakeVD) Sync(linux.SyncEvent) {}
func (f *fakeVD) SyncReport() {
	f.mu.Lock()
	f.events = append(f.events, rec{kind: "sync"})
	f.mu.Unlock()
}
func (f *fakeVD) PressKey(linux.Key)   {}
func (f *fakeVD) ReleaseKey(linux.Key) {}
func (f *fakeVD) PressButton(b linux.Button) {
	f.mu.Lock()
	f.events = append(f.events, rec{kind: "press", btn: b})
	f.mu.Unlock()
}
func (f *fakeVD) ReleaseButton(b linux.Button) {
	f.mu.Lock()
	f.events = append(f.events, rec{kind: "release", btn: b})
	f.mu.Unlock()
}
func (f *fakeVD) SendAbsoluteEvent(axis linux.AbsoluteAxis, value int32) {
	f.mu.Lock()
	f.events = append(f.events, rec{kind: "abs", axis: axis, value: value})
	f.mu.Unlock()
}
func (f *fakeVD) SendRelativeEvent(linux.RelativeAxis, int32) {}
func (f *fakeVD) SendMiscEvent(m linux.MiscEvent, value int32) {
	f.mu.Lock()
	f.events = append(f.events, rec{kind: "msc", msc: m, value: value})
	f.mu.Unlock()
}
func (f *fakeVD) SetLed(linux.Led, bool) {}
func (f *fakeVD) EventPath() string      { return "" }

// --- helpers for assertions ---

// syncs returns the indices of "sync" records (frame boundaries).
func (f *fakeVD) syncs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for i, e := range f.events {
		if e.kind == "sync" {
			out = append(out, i)
		}
	}
	return out
}

// frame returns the records belonging to frame n (0-indexed): the events up to
// and including the n-th SyncReport. The returned slice is a copy and safe to
// inspect even while the device continues to record.
func (f *fakeVD) frame(n int) []rec {
	f.mu.Lock()
	defer f.mu.Unlock()
	si := f.syncsLocked()
	if n >= len(si) {
		return nil
	}
	start := -1
	if n > 0 {
		start = si[n-1]
	}
	end := si[n] + 1
	cp := make([]rec, end-(start+1))
	copy(cp, f.events[start+1:end])
	return cp
}

// syncsLocked is syncs without locking; the caller must hold f.mu.
func (f *fakeVD) syncsLocked() []int {
	var out []int
	for i, e := range f.events {
		if e.kind == "sync" {
			out = append(out, i)
		}
	}
	return out
}

// absVal returns the value emitted for the given axis in a frame, or -1 if
// absent.
func absVal(frame []rec, axis linux.AbsoluteAxis) int32 {
	for _, e := range frame {
		if e.kind == "abs" && e.axis == axis {
			return e.value
		}
	}
	return -1
}

func hasSend(frame []rec, evType, code uint16, value int32) bool {
	for _, e := range frame {
		if e.kind == "send" && e.evType == evType && e.code == code && e.value == value {
			return true
		}
	}
	return false
}

func hasPress(frame []rec, btn linux.Button) bool {
	for _, e := range frame {
		if e.kind == "press" && e.btn == btn {
			return true
		}
	}
	return false
}

func hasRelease(frame []rec, btn linux.Button) bool {
	for _, e := range frame {
		if e.kind == "release" && e.btn == btn {
			return true
		}
	}
	return false
}

// --- event builders ---

func pdown(x, y float64) input.Event {
	return input.Event{Type: "pointerdown", T: []input.Touch{{ID: 1, X: x, Y: y}}}
}

func pmove(x, y float64) input.Event {
	return input.Event{Type: "pointermove", T: []input.Touch{{ID: 1, X: x, Y: y}}}
}

func pup() input.Event {
	return input.Event{Type: "pointerup", T: []input.Touch{{ID: 1, X: 0.5, Y: 0.5}}}
}

func bdown(btn string) input.Event {
	return input.Event{Type: "buttondown", Button: btn}
}

func bup(btn string) input.Event {
	return input.Event{Type: "buttonup", Button: btn}
}

func pressure(p float64) *float64 { return &p }
func tilt(t int) *int             { return &t }

// --- tests ---

// TestFirstStrokeProximityInThenTipDown verifies a pointerdown on an idle
// device emits a proximity-in (hover) frame followed by a tip-down frame, and
// that the tip-down pressure is floored at tipFloor so the stroke starts
// immediately even when the browser reports a low/zero pressure.
func TestFirstStrokeProximityInThenTipDown(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false)

	if err := d.ProcessEvent(pdown(0.5, 0.5)); err != nil {
		t.Fatal(err)
	}

	if got := len(vd.syncs()); got != 2 {
		t.Fatalf("expected 2 frames (proximity-in + tip-down), got %d", got)
	}

	// Frame 0: proximity-in hover frame.
	f0 := vd.frame(0)
	if !hasPress(f0, linux.BTN_TOOL_PEN) {
		t.Error("proximity-in frame should press BTN_TOOL_PEN")
	}
	if absVal(f0, linux.ABS_DISTANCE) != hoverDist {
		t.Errorf("proximity-in distance = %d, want %d", absVal(f0, linux.ABS_DISTANCE), hoverDist)
	}
	if absVal(f0, linux.ABS_PRESSURE) != 0 {
		t.Errorf("proximity-in pressure = %d, want 0", absVal(f0, linux.ABS_PRESSURE))
	}

	// Frame 1: tip-down. Pressure floored at tipFloor, BTN_TOUCH=1, distance 0.
	f1 := vd.frame(1)
	if !hasSend(f1, uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 1) {
		t.Error("tip-down frame should send BTN_TOUCH=1")
	}
	if absVal(f1, linux.ABS_DISTANCE) != 0 {
		t.Errorf("tip-down distance = %d, want 0", absVal(f1, linux.ABS_DISTANCE))
	}
	if got := absVal(f1, linux.ABS_PRESSURE); got != tipFloor {
		t.Errorf("tip-down pressure = %d, want tipFloor %d", got, tipFloor)
	}
}

// TestTipPressureFlooredWhileTipping verifies that while the tip is down, a
// pointermove with a pressure below tipFloor is floored at tipFloor (the
// stroke stays alive through pressure dips), and a pressure above tipFloor is
// passed through.
func TestTipPressureFlooredWhileTipping(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false)

	// Initial tip-down.
	if err := d.ProcessEvent(pdown(0.5, 0.5)); err != nil {
		t.Fatal(err)
	}
	vd.events = nil

	// Move with very low pressure -> floored at tipFloor.
	low := input.Event{Type: "pointermove", T: []input.Touch{{ID: 1, X: 0.5, Y: 0.5, Pressure: pressure(0.0)}}}
	if err := d.ProcessEvent(low); err != nil {
		t.Fatal(err)
	}
	last := vd.frame(len(vd.syncs()) - 1)
	if got := absVal(last, linux.ABS_PRESSURE); got != tipFloor {
		t.Errorf("low move pressure = %d, want floor %d", got, tipFloor)
	}
	if absVal(last, linux.ABS_DISTANCE) != 0 {
		t.Errorf("tipping move distance = %d, want 0", absVal(last, linux.ABS_DISTANCE))
	}

	vd.events = nil
	// Move with high pressure -> passed through.
	highP := 0.9
	high := input.Event{Type: "pointermove", T: []input.Touch{{ID: 1, X: 0.5, Y: 0.5, Pressure: pressure(highP)}}}
	if err := d.ProcessEvent(high); err != nil {
		t.Fatal(err)
	}
	last = vd.frame(len(vd.syncs()) - 1)
	want := int32(highP * float64(pressureMax))
	want = int32(float64(want)) // keep parity with setPen's truncation
	if got := absVal(last, linux.ABS_PRESSURE); got != want {
		t.Errorf("high move pressure = %d, want %d", got, want)
	}
}

// TestLiftStaysInProximity verifies a pointerup lifts the tip (BTN_TOUCH=0,
// back to hover pressure 0) but does NOT release BTN_TOOL_PEN — the tool stays
// in proximity so a subsequent stroke skips the proximity-in.
func TestLiftStaysInProximity(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false)

	if err := d.ProcessEvent(pdown(0.5, 0.5)); err != nil {
		t.Fatal(err)
	}
	vd.events = nil

	if err := d.ProcessEvent(pup()); err != nil {
		t.Fatal(err)
	}
	// pointerup emits a liftTip frame (BTN_TOUCH=0) then a hover frame.
	foundLift := false
	for i := 0; i < len(vd.syncs()); i++ {
		fr := vd.frame(i)
		if hasSend(fr, uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 0) {
			foundLift = true
			break
		}
	}
	if !foundLift {
		t.Error("pointerup should emit a frame with BTN_TOUCH=0 (lift the tip)")
	}
	last := vd.frame(len(vd.syncs()) - 1)
	if absVal(last, linux.ABS_DISTANCE) != hoverDist {
		t.Errorf("lift distance = %d, want %d (hover)", absVal(last, linux.ABS_DISTANCE), hoverDist)
	}
	if absVal(last, linux.ABS_PRESSURE) != 0 {
		t.Errorf("lift pressure = %d, want 0", absVal(last, linux.ABS_PRESSURE))
	}
	if hasRelease(last, linux.BTN_TOOL_PEN) {
		t.Error("pointerup should NOT release BTN_TOOL_PEN (stays in proximity)")
	}
}

// TestMoveAfterLiftEmitsHover verifies a pointermove while hovering (after a
// lift) emits a hover frame with distance=hoverDist, pressure=0 — even if the
// browser reports a synthetic pressure (which must be suppressed so libinput
// does not fire a spurious tip-down).
func TestMoveAfterLiftEmitsHover(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false)

	_ = d.ProcessEvent(pdown(0.5, 0.5))
	_ = d.ProcessEvent(pup())
	vd.events = nil

	// A finger pointer reports synthetic pressure 0.5; hover must force 0.
	xVal := 0.6
	move := input.Event{Type: "pointermove", T: []input.Touch{{ID: 1, X: xVal, Y: 0.6, Pressure: pressure(0.5)}}}
	if err := d.ProcessEvent(move); err != nil {
		t.Fatal(err)
	}
	last := vd.frame(len(vd.syncs()) - 1)
	if absVal(last, linux.ABS_PRESSURE) != 0 {
		t.Errorf("hover pressure = %d, want 0 (synthetic pressure suppressed)", absVal(last, linux.ABS_PRESSURE))
	}
	if absVal(last, linux.ABS_DISTANCE) != hoverDist {
		t.Errorf("hover distance = %d, want %d", absVal(last, linux.ABS_DISTANCE), hoverDist)
	}
	wantX := int32(xVal * float64(axisMax))
	if got := absVal(last, linux.ABS_X); got != wantX {
		t.Errorf("hover X = %d, want %d", got, wantX)
	}
}

// TestKeepAliveTogglesDistance verifies the hover keep-alive ticker emits
// frames that toggle ABS_DISTANCE between hoverDist and hoverDist+1, so the
// kernel forwards them (identical frames would be deduplicated). The ticker
// is only armed when keepAlive is enabled and the tool is hovering.
func TestKeepAliveTogglesDistance(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, true) // keepAlive ON

	_ = d.ProcessEvent(pdown(0.5, 0.5))
	_ = d.ProcessEvent(pup())
	// Arm the keep-alive by issuing a hover move, then wait for ticks.
	_ = d.ProcessEvent(pmove(0.5, 0.5))
	nFramesBefore := len(vd.syncs())

	// Wait long enough for at least two keep-alive ticks (keepAlivePeriod ~15ms).
	time.Sleep(60 * time.Millisecond)

	d.mu.Lock()
	d.stopKeepAlive()
	d.mu.Unlock()
	// Barrier: re-acquire d.mu so any in-flight ticker callback (which takes
	// d.mu) has finished before we read fakeVD, making the recorded frame count
	// deterministic.
	d.mu.Lock()
	d.mu.Unlock()

	// Keep-alive frames are those after the hover move.
	totalFrames := len(vd.syncs())
	if totalFrames-nFramesBefore < 2 {
		t.Fatalf("expected >=2 keep-alive frames, got %d", totalFrames-nFramesBefore)
	}
	var dists []int32
	for i := nFramesBefore; i < totalFrames; i++ {
		fr := vd.frame(i)
		dist := absVal(fr, linux.ABS_DISTANCE)
		if dist == -1 {
			continue
		}
		dists = append(dists, dist)
	}
	if len(dists) < 2 {
		t.Fatalf("expected >=2 keep-alive distance values, got %d", len(dists))
	}
	sawHover, sawHoverPlus1 := false, false
	for _, dd := range dists {
		if dd == hoverDist {
			sawHover = true
		}
		if dd == hoverDist+1 {
			sawHoverPlus1 = true
		}
	}
	if !sawHover || !sawHoverPlus1 {
		t.Errorf("keep-alive should toggle distance between %d and %d; got %v", hoverDist, hoverDist+1, dists)
	}
	// No keep-alive frame should press BTN_TOUCH (only the setup frames do).
	for i := nFramesBefore; i < totalFrames; i++ {
		fr := vd.frame(i)
		if hasSend(fr, uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 1) {
			t.Error("keep-alive frame should not press BTN_TOUCH")
		}
	}
}

// TestKeepAliveDisabledNoTicks verifies that with keepAlive off, hovering does
// not spawn keep-alive ticks (the device goes silent between strokes).
func TestKeepAliveDisabledNoTicks(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false) // keepAlive OFF

	_ = d.ProcessEvent(pdown(0.5, 0.5))
	_ = d.ProcessEvent(pup())
	vd.events = nil // ignore setup frames; count only post-move frames

	_ = d.ProcessEvent(pmove(0.5, 0.5))
	nAfterMove := len(vd.syncs())

	time.Sleep(60 * time.Millisecond)
	d.mu.Lock()
	d.stopKeepAlive()
	d.mu.Unlock()
	d.mu.Lock()
	d.mu.Unlock() // barrier: wait for any in-flight ticker callback

	// Only the hover-move frame should have been emitted; no extra keep-alive
	// frames should appear after the wait.
	if len(vd.syncs()) != nAfterMove {
		t.Errorf("keepAlive off should not emit keep-alive ticks; syncs went %d -> %d", nAfterMove, len(vd.syncs()))
	}
}

// TestButtonMapping verifies the L/M/R on-screen buttons map to the correct
// tablet tool buttons: L=BTN_TOUCH (tip), R=BTN_STYLUS (right/barrel),
// M=BTN_STYLUS2 (middle/barrel2). Each name is exercised as a down-then-up
// pair on a fresh device so the tip (for L) is actually down before the up.
func TestButtonMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		btn  linux.Button
	}{
		{"left", linux.BTN_TOUCH},
		{"right", linux.BTN_STYLUS},
		{"middle", linux.BTN_STYLUS2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vd := &fakeVD{}
			d := newTestDevice(vd, false)

			// Down: some frame must contain the mapped button = 1.
			if err := d.ProcessEvent(bdown(tc.name)); err != nil {
				t.Fatal(err)
			}
			foundDown := false
			for i := 0; i < len(vd.syncs()); i++ {
				if hasSend(vd.frame(i), uint16(linux.EV_KEY), uint16(tc.btn), 1) {
					foundDown = true
					break
				}
			}
			if !foundDown {
				t.Errorf("%s down: no frame sent %d=1", tc.name, tc.btn)
			}

			// Up: the last frame must contain the mapped button = 0.
			if err := d.ProcessEvent(bup(tc.name)); err != nil {
				t.Fatal(err)
			}
			last := vd.frame(len(vd.syncs()) - 1)
			if !hasSend(last, uint16(linux.EV_KEY), uint16(tc.btn), 0) {
				t.Errorf("%s up: last frame %+v missing %d=0", tc.name, last, tc.btn)
			}
		})
	}
}

// TestButtonPressProximityIn verifies a button event on an idle device first
// brings the tool into proximity (BTN_TOOL_PEN pressed) before the button
// frame, so the compositor sees the tool arrive.
func TestButtonPressProximityIn(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false)

	if err := d.ProcessEvent(bdown("right")); err != nil {
		t.Fatal(err)
	}
	si := vd.syncs()
	if len(si) < 2 {
		t.Fatalf("expected >=2 frames (proximity-in + button), got %d", len(si))
	}
	f0 := vd.frame(0)
	if !hasPress(f0, linux.BTN_TOOL_PEN) {
		t.Error("button event on idle device should proximity-in first")
	}
}

// TestSetActiveFalseProximityOut verifies SetActive(false) releases the tool
// (proximity-out): tip lifted and BTN_TOOL_PEN released.
func TestSetActiveFalseProximityOut(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false)

	_ = d.ProcessEvent(pdown(0.5, 0.5))
	vd.events = nil

	d.SetActive(false)
	last := vd.frame(len(vd.syncs()) - 1)
	if hasSend(last, uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 1) {
		t.Error("SetActive(false) should lift the tip (no BTN_TOUCH=1)")
	}
	if !hasRelease(last, linux.BTN_TOOL_PEN) {
		t.Error("SetActive(false) should release BTN_TOOL_PEN (proximity-out)")
	}
}

// TestResetProximityOut verifies Reset (client disconnect) lifts the tip and
// leaves proximity cleanly.
func TestResetProximityOut(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false)

	_ = d.ProcessEvent(pdown(0.5, 0.5))
	vd.events = nil

	d.Reset()
	last := vd.frame(len(vd.syncs()) - 1)
	if !hasRelease(last, linux.BTN_TOOL_PEN) {
		t.Error("Reset should release BTN_TOOL_PEN (proximity-out)")
	}
	if hasSend(last, uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 1) {
		t.Error("Reset should not leave the tip down")
	}
}

// TestStaleTipRecovery verifies that a pointerdown when the device still
// thinks a tip is down (a previous stroke whose tip-up was lost) first lifts
// the tip, so the kernel sees a BTN_TOUCH 0->1 transition and the stroke is
// not invisible.
func TestStaleTipRecovery(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false)

	_ = d.ProcessEvent(pdown(0.5, 0.5))
	vd.events = nil

	// Force the device into a stale "tip down" state without a tip-up, then
	// start a new stroke.
	d.mu.Lock()
	d.tipDown = true
	d.mu.Unlock()

	if err := d.ProcessEvent(pdown(0.6, 0.6)); err != nil {
		t.Fatal(err)
	}
	// The first emitted frame should lift the stale tip (BTN_TOUCH=0).
	f0 := vd.frame(0)
	if !hasSend(f0, uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 0) {
		t.Error("stale-tip recovery should first emit BTN_TOUCH=0")
	}
	// And a later frame should drop the tip again (BTN_TOUCH=1).
	foundDown := false
	for i := 0; i < len(vd.syncs()); i++ {
		fr := vd.frame(i)
		if hasSend(fr, uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 1) {
			foundDown = true
		}
	}
	if !foundDown {
		t.Error("stale-tip recovery should re-drop the tip (BTN_TOUCH=1)")
	}
}

// TestTiltForwarding verifies pressure/tilt from the browser PointerEvent are
// forwarded to the tablet axes (only meaningful for the tablet device).
func TestTiltForwarding(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false)

	ev := input.Event{Type: "pointerdown", T: []input.Touch{{
		ID: 1, X: 0.5, Y: 0.5, Pressure: pressure(0.5), TiltX: tilt(30), TiltY: tilt(-20),
	}}}
	if err := d.ProcessEvent(ev); err != nil {
		t.Fatal(err)
	}
	// Inspect the tip-down frame (frame 1).
	f1 := vd.frame(1)
	if got := absVal(f1, linux.ABS_TILT_X); got != 30 {
		t.Errorf("tiltX = %d, want 30", got)
	}
	if got := absVal(f1, linux.ABS_TILT_Y); got != -20 {
		t.Errorf("tiltY = %d, want -20", got)
	}
}

// TestUnknownEventTypeReturnsError verifies an unknown event type is rejected
// rather than silently ignored.
func TestUnknownEventTypeReturnsError(t *testing.T) {
	vd := &fakeVD{}
	d := newTestDevice(vd, false)
	err := d.ProcessEvent(input.Event{Type: "frobnicate"})
	if err == nil {
		t.Fatal("expected error for unknown event type")
	}
}
