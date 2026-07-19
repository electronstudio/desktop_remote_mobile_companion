// Package tablet creates a virtual Linux graphics tablet via uinput and maps
// browser touch events to absolute screen coordinates.
//
// The device mimics a real pen tablet (like a Wacom Intuos or Surface Pen):
// touch acts as the pen hovering over the surface (moving the cursor without
// clicking), and the client's L/M/R buttons act as the pen tip and barrel
// buttons which the desktop maps to left / right / middle clicks.
//
// The axis resolution is set before the device is created using the
// UI_ABS_SETUP ioctl (Linux 4.16+) via our local fork of
// github.com/jbdemonte/virtual-device in third_party/virtual-device/, so
// libinput sees the resolution on its first probe. See AGENTS.md.
package tablet

import (
	"fmt"
	"sync"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
	virtual_device "github.com/jbdemonte/virtual-device"
	"github.com/jbdemonte/virtual-device/linux"
)

const (
	axisMax    = 32767
	resolution = 200  // units/mm — set via UI_ABS_SETUP at device creation
	toolSerial = 1    // MSC_SERIAL tool serial for tracking
	pressure   = 2048 // pressure used for tip (left click)
	hoverDist  = 10   // ABS_DISTANCE while hovering (keeps tool in proximity)
)

// Device is a virtual single-touch graphics tablet.
type Device struct {
	vd      virtual_device.VirtualDevice
	mu      sync.Mutex
	inRange bool
	x, y    int32
}

// New creates and registers a virtual graphics tablet.
func New() (*Device, error) {
	vd := virtual_device.NewVirtualDevice().
		WithBusType(linux.BUS_VIRTUAL).
		WithVendor(0x1234).
		WithProduct(0x5679).
		WithVersion(0x0001).
		WithName("Desktop Remote Mobile Companion Tablet").
		WithAbsAxes([]virtual_device.AbsAxis{
			{Axis: linux.ABS_X, Min: 0, Value: 0, Max: axisMax, Resolution: resolution},
			{Axis: linux.ABS_Y, Min: 0, Value: 0, Max: axisMax, Resolution: resolution},
			{Axis: linux.ABS_PRESSURE, Min: 0, Value: 0, Max: 4096, IsUnidirectional: true},
			{Axis: linux.ABS_DISTANCE, Min: 0, Value: 0, Max: 255},
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

	return &Device{vd: vd}, nil
}

// Close unregisters the virtual device.
func (d *Device) Close() error {
	return d.vd.Unregister()
}

// ProcessEvent applies a browser event to the virtual tablet.
func (d *Device) ProcessEvent(ev input.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch ev.Type {
	case "touchstart", "pointerdown":
		if len(ev.T) == 0 {
			return nil
		}
		t := ev.T[0]
		d.x = int32(t.X * float64(axisMax))
		d.y = int32(t.Y * float64(axisMax))
		if !d.inRange {
			d.proximityIn()
		} else {
			d.emitHover()
		}
	case "touchmove", "pointermove":
		if len(ev.T) == 0 {
			return nil
		}
		t := ev.T[0]
		d.x = int32(t.X * float64(axisMax))
		d.y = int32(t.Y * float64(axisMax))
		if !d.inRange {
			d.proximityIn()
		} else {
			d.emitHover()
		}
	case "touchend", "touchcancel", "pointerup", "pointercancel":
		if d.inRange {
			d.proximityOut()
		}
	case "buttondown", "buttonup":
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
			// Left click = tip contact: distance=0, pressure>0, BTN_TOUCH=1.
			if pressed {
				d.emitFrame(0, pressure, linux.BTN_TOUCH, 1)
			} else {
				d.emitFrame(hoverDist, 0, linux.BTN_TOUCH, 0)
			}
		} else {
			// Barrel buttons (BTN_STYLUS / BTN_STYLUS2).
			d.emitFrame(hoverDist, 0, btn, boolToInt(pressed))
		}
	default:
		return fmt.Errorf("unknown event type: %q", ev.Type)
	}

	return nil
}

// proximityIn brings the tool into range and sends a complete hover frame
// (with SyncReport) so the compositor processes proximity-in before any
// subsequent tip/button frame.
func (d *Device) proximityIn() {
	d.vd.SendMiscEvent(linux.MSC_SERIAL, toolSerial)
	d.vd.Send(uint16(linux.EV_ABS), uint16(linux.ABS_MISC), 0)
	d.vd.PressButton(linux.BTN_TOOL_PEN)
	d.vd.SendAbsoluteEvent(linux.ABS_X, d.x)
	d.vd.SendAbsoluteEvent(linux.ABS_Y, d.y)
	d.vd.SendAbsoluteEvent(linux.ABS_DISTANCE, hoverDist)
	d.vd.SendAbsoluteEvent(linux.ABS_PRESSURE, 0)
	d.vd.SyncReport()
	d.inRange = true
}

func (d *Device) proximityOut() {
	d.vd.ReleaseButton(linux.BTN_TOOL_PEN)
	d.vd.SendMiscEvent(linux.MSC_SERIAL, 0)
	d.inRange = false
	d.vd.SyncReport()
}

// emitHover sends an axis frame with distance>0 and pressure=0 (pen hovering).
func (d *Device) emitHover() {
	d.emitFrame(hoverDist, 0, 0, 0)
}

// emitFrame sends a complete axis frame with an optional button change.
func (d *Device) emitFrame(distance, press int32, btn linux.Button, btnVal int32) {
	d.vd.SendAbsoluteEvent(linux.ABS_X, d.x)
	d.vd.SendAbsoluteEvent(linux.ABS_Y, d.y)
	d.vd.SendAbsoluteEvent(linux.ABS_DISTANCE, distance)
	d.vd.SendAbsoluteEvent(linux.ABS_PRESSURE, press)
	if btn != 0 {
		d.vd.Send(uint16(linux.EV_KEY), uint16(btn), btnVal)
	}
	d.vd.SyncReport()
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
