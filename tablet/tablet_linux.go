//go:build linux

// Package tablet creates a virtual Linux graphics tablet via uinput and maps
// browser touch events to absolute screen coordinates.
//
// This file is the Linux (uinput) backend. The cross-platform Device
// interface and New constructor live in tablet.go.
//
// The device mimics a real pen tablet (like a Wacom Intuos or Surface Pen):
// touching the surface is a pen-tip contact (the cursor follows and the
// application draws, with live pressure and tilt forwarded from the
// browser), and the client's L/M/R on-screen buttons act as the pen tip and
// barrel buttons which the desktop maps to left / right / middle clicks. A
// real pen keeps hovering between strokes; since a phone touchscreen has no
// real hover, the server keeps the tool artificially in proximity between
// strokes with a keep-alive ticker (see keepAlivePeriod) so the compositor
// does not drop the next stroke.
//
// The axis resolution is set before the device is created using the
// UI_ABS_SETUP ioctl (Linux 4.16+) via our local fork of
// github.com/jbdemonte/virtual-device in third_party/virtual-device/, so
// libinput sees the resolution on its first probe. See AGENTS.md.
package tablet

import (
	"fmt"
	"sync"
	"time"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
	virtual_device "github.com/jbdemonte/virtual-device"
	"github.com/jbdemonte/virtual-device/linux"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

const (
	axisMax             = 32767
	resolution          = 200  // units/mm — set via UI_ABS_SETUP at device creation
	toolSerial          = 1    // MSC_SERIAL tool serial for tracking
	hoverDist           = 10   // ABS_DISTANCE while hovering (keeps tool in proximity)
	pressureMax         = 4096 // ABS_PRESSURE maximum (matches the axis range below)
	tiltMin             = -90  // ABS_TILT_X/Y range, degrees (matches PointerEvent tiltX/tiltY)
	tiltMax             = 90
	proximityToTipDelay = 8 * time.Millisecond // gap between a fresh proximity-in and tip-down; see beginStroke
	// libinput derives tablet tip state from ABS_PRESSURE crossing a threshold
	// (it ignores BTN_TOUCH when an ABS_PRESSURE axis exists), defaulting to ~5%
	// of the axis range with hysteresis. A pressure-sensitive stylus often
	// reports a very low pressure (or 0) for the first samples after touching
	// and during light contact, which drops ABS_PRESSURE below libinput's
	// threshold mid-stroke and lifts the tip (the stroke dies). To keep a
	// stroke alive through brief pressure dips we floor the emitted pressure
	// at tipFloor while the tip is down: once down, the reported pressure is
	// never allowed below tipFloor until the pen lifts. tipFloor sits above
	// libinput's ~5% (205) default threshold so the tip stays logically down.
	// TODO: test if this is still necessary
	tipFloor = 256 // emitted-pressure floor while tip is down (>= libinput ~5% of 4096)

	// libinput has a "no-proximity-out" quirk: for tablet tools that go silent
	// (no axis events) while still in proximity, it forces a proximity-out
	// after a short timeout, then re-injects a proximity-in on the next event.
	// That proximity-out->in cycle triggers a multi-second cooldown in the
	// GNOME/Mutter (Wayland) compositor during which the tablet tool's pointer
	// is not delivered at all (a stroke does nothing for a few seconds, then
	// starts drawing mid-way). A real Wacom pen avoids this by continuously
	// streaming axis samples while hovering. We are a phone touchscreen with
	// no real hover, so after a stroke lifts we would otherwise go dead silent
	// until the next touch. keepAlivePeriod is how often we re-emit the current
	// hover frame while the tool is in range and not tipping, keeping the tool
	// "alive" so libinput's quirk timer never fires and Mutter never sees a
	// proximity-out->in.
	keepAlivePeriod = 15 * time.Millisecond
)

// device is a virtual single-touch graphics tablet (the Linux uinput
// backend). It implements the tablet.Device interface.
type device struct {
	vd          virtual_device.VirtualDevice
	mu          sync.Mutex
	inRange     bool
	tipDown     bool
	keepAliveOn bool        // whether the hover keep-alive ticker is enabled (Mutter workaround)
	keepAlive   *time.Timer // re-emits the hover frame while hovering (see keepAlivePeriod)
	kaToggle    bool        // alternates ABS_DISTANCE so keep-alive frames are not deduped
	x, y        int32
	pressure    int32 // scaled 0..pressureMax, from the browser (0 while hovering)
	tiltX       int32 // degrees, tiltMin..tiltMax
	tiltY       int32 // degrees, tiltMin..tiltMax
}

// newDevice creates and registers a virtual graphics tablet (the Linux
// uinput backend) and returns it as a Device. It is the platform-specific
// constructor called by the cross-platform New in tablet.go.
//
// When keepAlive is true the server keeps the tool artificially in proximity
// between strokes (see keepAlivePeriod), working around a GNOME/Mutter
// (Wayland) cooldown that drops strokes after a proximity-out. It also keeps
// the cursor grabbed while the tablet panel is active; pass false on
// compositors without that cooldown (e.g. wlroots) to avoid the perpetual
// hover and let the mouse work.
func newDevice(keepAlive bool) (Device, error) {
	// attempt to raise caps for situation where we dont have sudo
	// and we dont have input group access to
	// uinput but we do have libcap ability to get sys admin caps
	orig := cap.GetProc()
	defer orig.SetProc() // restore original caps on exit.
	c, err := orig.Dup()
	if err == nil {
		if err := c.SetFlag(cap.Effective, true, cap.DAC_OVERRIDE); err == nil {
			c.SetProc()
		}
	}

	// Spoof a real pen tablet so libwacom (and therefore the GNOME Settings
	// "Graphics Tablets" panel) recognises the device. GNOME lists only
	// tablets libwacom can describe; libwacom's get_device_info() rejects any
	// bus it cannot map to USB/Bluetooth/I2C, and a BUS_VIRTUAL (0x06) device
	// with no UINPUT_SUBSYSTEM udev property yields "Unsupported bus
	// 'unknown'", so even GNOME 47+'s generic fallback never shows it. We
	// pretend to be a "One by Wacom (medium)" (CTL-671, usb 056a:0301): a
	// pen-only tablet with no pad buttons, no touch and pressure+tilt —
	// exactly the capabilities we emulate — so the panel shows it with the
	// correct options. The friendly Name is cosmetic only; libwacom matches
	// on bus/vid/pid, not the name.
	vd := virtual_device.NewVirtualDevice().
		WithBusType(linux.BUS_USB).
		WithVendor(0x056a).  // Wacom Co., Ltd
		WithProduct(0x0301). // One by Wacom (medium), CTL-671
		WithVersion(0x0001).
		WithName("Desktop Remote Mobile Companion Tablet").
		WithAbsAxes([]virtual_device.AbsAxis{
			{Axis: linux.ABS_X, Min: 0, Value: 0, Max: axisMax, Resolution: resolution},
			{Axis: linux.ABS_Y, Min: 0, Value: 0, Max: axisMax, Resolution: resolution},
			{Axis: linux.ABS_PRESSURE, Min: 0, Value: 0, Max: pressureMax, IsUnidirectional: true},
			{Axis: linux.ABS_DISTANCE, Min: 0, Value: 0, Max: 255},
			{Axis: linux.ABS_TILT_X, Min: tiltMin, Value: 0, Max: tiltMax},
			{Axis: linux.ABS_TILT_Y, Min: tiltMin, Value: 0, Max: tiltMax},
			{Axis: linux.ABS_MISC, Min: 0, Value: 0, Max: 65535},
		}).
		WithButtons([]linux.Button{
			linux.BTN_TOOL_PEN,
			linux.BTN_TOOL_RUBBER,
			linux.BTN_TOUCH,
			linux.BTN_STYLUS,
			linux.BTN_STYLUS2,
		}).
		WithMiscEvents([]linux.MiscEvent{
			linux.MSC_SERIAL,
		})

	if err := vd.Register(); err != nil {
		return nil, fmt.Errorf("register virtual tablet: %w", err)
	}

	return &device{vd: vd, keepAliveOn: keepAlive}, nil
}

// newTestDevice builds a tablet.device wired to a fake VirtualDevice,
// bypassing newDevice()/Register() so tests need no /dev/uinput. It is the
// test-only constructor used by tablet_linux_test.go.
func newTestDevice(vd virtual_device.VirtualDevice, keepAlive bool) *device {
	return &device{vd: vd, keepAliveOn: keepAlive}
}

// Close unregisters the virtual device.
func (d *device) Close() error {
	return d.vd.Unregister()
}

// ProcessEvent applies a browser event to the virtual tablet.
func (d *device) ProcessEvent(ev input.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch ev.Type {
	case "pointerdown":
		// Pen touches the surface: bring the tool into proximity (hover)
		// then drop the tip (contact). The two-frame sequence matches a real
		// tablet and lets the compositor process proximity-in before the
		// tip-down. beginStroke also recovers from stale state left by a
		// previous stroke whose tip-up was lost (e.g. a dropped data channel
		// or a client that disconnected mid-stroke): if the kernel still
		// thinks the tip is down, dropTip would no-op and no TABLET_TOOL_TIP
		// event would fire, so the next stroke would be invisible until the
		// user lifted and re-touched. beginStroke forces a clean transition.
		if len(ev.T) == 0 {
			return nil
		}
		d.setPen(ev.T[0])
		d.beginStroke()
	case "pointermove":
		// Pen moved. While tipping, update axes with live pressure/tilt; while
		// hovering, emit a hover frame and (re)arm the keep-alive so the tool
		// does not go silent. (A move also resets the keep-alive timer.)
		if len(ev.T) == 0 {
			return nil
		}
		d.setPen(ev.T[0])
		if !d.inRange {
			d.proximityIn()
			d.dropTip()
		} else if d.tipDown {
			d.emitFrame(0, d.tipPressure(), 0, 0)
		} else {
			d.emitHover()
			d.armKeepAlive() // (re)arm: this move resets the silence timer
		}
	case "pointerup", "pointercancel":
		// Pen lifts off the surface: release the tip back to a hover frame but
		// STAY in proximity. A real Wacom pen does not leave the tablet's
		// sensing area between strokes; it keeps hovering and keeps streaming
		// axis samples. We have no real hover (phone touchscreen), so after
		// lifting we would go dead silent until the next touch — and libinput's
		// no-proximity-out quirk then forces a proximity-out, which triggers a
		// multi-second Mutter cooldown on the next proximity-in (a stroke does
		// nothing for a few seconds, then starts drawing mid-way). To avoid
		// that we keep the tool "alive" by re-emitting the hover frame on a
		// timer (armKeepAlive) until the next stroke or a real lift.
		if d.inRange {
			if d.tipDown {
				d.liftTip()
			}
			d.emitHover()
			d.armKeepAlive()
		}
	case "buttondown", "buttonup": // TODO do we still need these?
		btn, ok := buttonMap(ev.Button)
		if !ok {
			return fmt.Errorf("unknown button: %q", ev.Button)
		}
		pressed := ev.Type == "buttondown"

		// Ensure tool is in proximity. proximityIn sends its own sync'd
		// hover frame so the compositor sees the tool arrive before any
		// tip/button event in a subsequent frame.
		if !d.inRange {
			d.proximityIn()
		}

		if btn == linux.BTN_TOUCH {
			// On-screen L button = explicit tip contact (a click without
			// a touch/draw). It shares BTN_TOUCH with a real pen tip, so it
			// conflicts with an active pen contact; in practice the two are
			// not used together.
			if pressed {
				d.dropTip()
			} else {
				d.liftTip()
				d.armKeepAlive()
			}
		} else {
			// Barrel buttons (BTN_STYLUS / BTN_STYLUS2), emitted while
			// hovering (or over an active tip).
			distance := int32(hoverDist)
			press := int32(0)
			if d.tipDown {
				distance = 0
				press = d.tipPressure()
			}
			d.emitFrame(distance, press, btn, boolToInt(pressed))
		}
	default:
		return fmt.Errorf("unknown event type: %q", ev.Type)
	}

	return nil
}

// proximityIn brings the tool into range and sends a complete hover frame
// (with SyncReport) so the compositor processes proximity-in before any
// subsequent tip/button frame.
func (d *device) proximityIn() {
	d.vd.SendMiscEvent(linux.MSC_SERIAL, toolSerial)
	d.vd.Send(uint16(linux.EV_ABS), uint16(linux.ABS_MISC), 0)
	d.vd.PressButton(linux.BTN_TOOL_PEN)
	d.vd.SendAbsoluteEvent(linux.ABS_X, d.x)
	d.vd.SendAbsoluteEvent(linux.ABS_Y, d.y)
	d.vd.SendAbsoluteEvent(linux.ABS_DISTANCE, hoverDist)
	d.vd.SendAbsoluteEvent(linux.ABS_PRESSURE, 0)
	d.vd.SendAbsoluteEvent(linux.ABS_TILT_X, d.tiltX)
	d.vd.SendAbsoluteEvent(linux.ABS_TILT_Y, d.tiltY)
	d.vd.SyncReport()
	d.inRange = true
}

func (d *device) proximityOut() {
	d.stopKeepAlive()
	if d.tipDown {
		// Safety: ensure the tip is released before the tool leaves range.
		d.vd.Send(uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 0)
		d.tipDown = false
	}
	d.vd.ReleaseButton(linux.BTN_TOOL_PEN)
	d.vd.SendMiscEvent(linux.MSC_SERIAL, 0)
	d.inRange = false
	d.vd.SyncReport()
}

// dropTip drops the pen tip onto the surface: distance=0, pressure>0,
// BTN_TOUCH=1. The emitted pressure is the pen's reported pressure floored
// at tipFloor (see tipPressure), so even a very light/zero first sample
// crosses libinput's tip threshold and the stroke starts immediately.
func (d *device) dropTip() {
	if d.tipDown {
		return
	}
	d.stopKeepAlive()
	d.emitFrame(0, d.tipPressure(), linux.BTN_TOUCH, 1)
	d.tipDown = true
}

// beginStroke brings the pen into contact. It first recovers from stale
// state: if the device still thinks a tip is down (a previous stroke whose
// tip-up was lost, e.g. the client disconnected mid-stroke), it lifts the tip
// so the kernel sees a real BTN_TOUCH 1->0->1 transition and libinput fires a
// fresh TIP_DOWN; otherwise the new stroke would be invisible until the user
// lifted and re-touched. It then brings the tool into proximity (if not
// already) and drops the tip.
func (d *device) beginStroke() {
	if d.inRange && d.tipDown {
		d.liftTip()
	}
	if !d.inRange {
		d.proximityIn()
		// A real pen descends through hover before touching the surface, so
		// there is a small gap between the tool entering proximity and the
		// tip making contact. We emit proximity-in and tip-down in the same
		// pointerdown call; briefly waiting after the proximity-in frame gives
		// the compositor a sync point to register the tool before the tip-down
		// frame is emitted. (This only runs on the first stroke after an idle
		// proximity-out; subsequent strokes stay in proximity and skip this.)
		time.Sleep(proximityToTipDelay)
	}
	d.dropTip()
}

// Reset releases the tool when a client disconnects: lift the tip and leave
// proximity cleanly (a real BTN_TOOL_PEN=0). This is the one place we
// intentionally proximity-out — not per stroke (the keep-alive ticker keeps
// the tool alive between strokes so libinput never forces a proximity-out
// there), but on a genuine disconnect, where no client will send further
// events. Streaming keep-alives after disconnect would spin forever, and
// leaving the tool silently in-range would let libinput's quirk force a
// proximity-out anyway; a clean proximity-out here is better. The next
// connection's first stroke does a fresh proximity-in + tip-down; being a
// lone first stroke (not rapid after a recent one) it does not hit Mutter's
// cooldown.
func (d *device) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopKeepAlive()
	if d.inRange {
		if d.tipDown {
			d.liftTip()
		}
		d.proximityOut()
	}
}

// SetActive is the control handler for the client's "activate" message.
// When the tablet panel becomes the active panel the client sends active=true;
// when it is swiped away it sends active=false. On false the tool is released
// (proximity-out) so the system mouse works again while the user is not
// drawing. On true nothing is done — the next touch does a fresh proximity-in,
// which (being a lone first stroke) the compositor handles.
func (d *device) SetActive(active bool) {
	if !active {
		d.Reset()
	}
}

// armKeepAlive (re)arms the hover keep-alive ticker. While the tool is in
// proximity and not tipping (i.e. hovering), the ticker re-emits the current
// hover frame every keepAlivePeriod so libinput never sees the tool go silent
// — which would trigger its no-proximity-out quirk (a forced proximity-out
// followed by a re-injected proximity-in on the next event) and the
// multi-second Mutter cooldown that follows. It is a no-op if the tip is
// down or the tool is out of range. Calling it again (e.g. on each hover
// pointermove) just resets the timer.
func (d *device) armKeepAlive() {
	if !d.keepAliveOn || !d.inRange || d.tipDown {
		d.stopKeepAlive()
		return
	}
	if d.keepAlive != nil {
		d.keepAlive.Reset(keepAlivePeriod)
		return
	}
	d.keepAlive = time.AfterFunc(keepAlivePeriod, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if !d.inRange || d.tipDown {
			d.keepAlive = nil
			return
		}
		d.kaToggle = !d.kaToggle
		d.emitKeepAlive(d.kaToggle)
		// Re-arm for the next interval.
		if d.keepAlive != nil {
			d.keepAlive.Reset(keepAlivePeriod)
		}
	})
}

// stopKeepAlive cancels the hover keep-alive ticker (e.g. on tip-down, on a
// real lift, or on disconnect). It is safe to call when no ticker is armed.
func (d *device) stopKeepAlive() {
	if d.keepAlive != nil {
		d.keepAlive.Stop()
		d.keepAlive = nil
	}
}

// liftTip lifts the pen tip off the surface: BTN_TOUCH=0, back to a hover
// frame (distance=hoverDist, pressure=0). The caller is responsible for
// (re)arming the keep-alive ticker if the pen should keep hovering.
func (d *device) liftTip() {
	if !d.tipDown {
		return
	}
	d.emitFrame(hoverDist, 0, linux.BTN_TOUCH, 0)
	d.tipDown = false
}

// emitHover sends an axis frame for a hovering pen: distance=hoverDist,
// pressure=0, with the pen's tilt. A real pen reports pressure 0 while
// hovering (pressure only becomes meaningful on tip contact). Emitting a
// non-zero hover pressure (e.g. a touch pointer's synthetic 0.5) makes
// libinput cross its tip-down threshold and fire spurious TIP_DOWN events,
// so hover pressure is forced to 0 regardless of what the browser reports.
func (d *device) emitHover() {
	d.emitFrame(hoverDist, 0, 0, 0)
}

// emitKeepAlive emits a hover frame that differs from the previous one so the
// kernel forwards it. The Linux input protocol is stateful: unchanged axis
// values are deduplicated and never reach userspace, so a stream of identical
// hover frames (all distance=hoverDist, pressure=0, same x/y/tilt) produces
// zero evdev events and libinput still sees the tool go silent — which trips
// its no-proximity-out quirk. A real Wacom hovers with sub-millimeter jitter;
// we fake that by toggling ABS_DISTANCE between hoverDist and hoverDist+1 on
// alternate ticks. Both are nonzero (pen hovering, not touching) so libinput
// keeps the tool in proximity, but the change guarantees a real event each
// tick. (We nudge distance rather than x/y/tilt so the on-screen cursor stays
// put.)
func (d *device) emitKeepAlive(toggle bool) {
	dist := int32(hoverDist)
	if toggle {
		dist = hoverDist + 1
	}
	d.emitFrame(dist, 0, 0, 0)
}

// emitFrame sends a complete axis frame with an optional button change.
func (d *device) emitFrame(distance, press int32, btn linux.Button, btnVal int32) {
	d.vd.SendAbsoluteEvent(linux.ABS_X, d.x)
	d.vd.SendAbsoluteEvent(linux.ABS_Y, d.y)
	d.vd.SendAbsoluteEvent(linux.ABS_DISTANCE, distance)
	d.vd.SendAbsoluteEvent(linux.ABS_PRESSURE, press)
	d.vd.SendAbsoluteEvent(linux.ABS_TILT_X, d.tiltX)
	d.vd.SendAbsoluteEvent(linux.ABS_TILT_Y, d.tiltY)
	if btn != 0 {
		d.vd.Send(uint16(linux.EV_KEY), uint16(btn), btnVal)
	}
	d.vd.SyncReport()
}

// setPen updates the cached pen state (position, pressure, tilt) from a
// browser touch sample. Absent pressure is treated as 0 (hover); absent tilt
// as 0 degrees. This keeps "not reported" distinguishable from a real zero
// (see the input.Touch docs) at the protocol boundary.
func (d *device) setPen(t input.Touch) {
	d.x = int32(t.X * float64(axisMax))
	d.y = int32(t.Y * float64(axisMax))
	if t.Pressure != nil {
		p := *t.Pressure
		if p < 0 {
			p = 0
		} else if p > 1 {
			p = 1
		}
		d.pressure = int32(p * float64(pressureMax))
	} else {
		d.pressure = 0
	}
	d.tiltX = clampTilt(t.TiltX)
	d.tiltY = clampTilt(t.TiltY)
}

// tipPressure returns the pressure to emit for a tip (left-click) contact.
// While the tip is down the reported pressure is floored at tipFloor so a
// brief dip (including a spurious 0 from the browser) does not drop
// ABS_PRESSURE below libinput's tip threshold and lift the tip mid-stroke.
// On the down transition itself we use the pen's real pressure if it is
// already above the floor, otherwise the floor, so the tip-down event
// crosses the threshold immediately.
func (d *device) tipPressure() int32 {
	if d.pressure > tipFloor {
		return d.pressure
	}
	return tipFloor
}

func clampTilt(v *int) int32 {
	if v == nil {
		return 0
	}
	t := int32(*v)
	if t < tiltMin {
		return tiltMin
	}
	if t > tiltMax {
		return tiltMax
	}
	return t
}

func boolToInt(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

// buttonMap maps client button names to tablet tool buttons.
// On most desktops: BTN_TOUCH = left click (tip),
// BTN_STYLUS = right click (barrel), BTN_STYLUS2 = middle click (barrel 2).
func buttonMap(name string) (linux.Button, bool) {
	switch name {
	case "left":
		return linux.BTN_TOUCH, true
	case "middle":
		return linux.BTN_STYLUS2, true
	case "right":
		return linux.BTN_STYLUS, true
	default:
		return 0, false
	}
}
