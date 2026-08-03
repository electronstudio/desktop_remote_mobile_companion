//go:build linux

// Package trackpad creates a virtual Linux multitouch trackpad via uinput
// and maps browser touch events to kernel input events.
//
// This file is the Linux (uinput) backend. The cross-platform Device interface
// and New constructor live in trackpad.go.
package trackpad

import (
	"fmt"
	"sort"
	"sync"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
	virtual_device "github.com/jbdemonte/virtual-device"
	"github.com/jbdemonte/virtual-device/linux"
	"github.com/jbdemonte/virtual-device/touchpad"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

const (
	maxSlots   = 10
	axisMax    = 8191
	resolution = 80 // ~102mm physical touchpad size, realistic units/mm
)

// device is a virtual multitouch trackpad (the Linux uinput backend). It
// implements the trackpad.Device interface.
type device struct {
	tp       touchpad.VirtualTouchpad
	mu       sync.Mutex
	active   map[int]touchpad.TouchSlot // browser touch id -> slot state
	slots    [maxSlots]bool             // true if slot is in use
	touching bool                       // at least one contact is currently down
}

// newDevice creates and registers a virtual multitouch trackpad (the Linux
// uinput backend) and returns it as a Device. It is the platform-specific
// constructor called by the cross-platform New in trackpad.go.
func newDevice() (Device, error) {
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

	vd := virtual_device.NewVirtualDevice().
		WithBusType(linux.BUS_USB).
		WithVendor(0x1234).
		WithProduct(0x5678).
		WithVersion(0x0001).
		WithName("Desktop Remote Mobile Companion Touchpad")

	tp := touchpad.NewVirtualTouchpadFactory().
		WithDevice(vd).
		WithAxes([]virtual_device.AbsAxis{
			{Axis: linux.ABS_X, Min: 0, Value: 0, Max: axisMax, Resolution: resolution},
			{Axis: linux.ABS_Y, Min: 0, Value: 0, Max: axisMax, Resolution: resolution},
			{Axis: linux.ABS_PRESSURE, Min: 0, Value: 0, Max: 255, IsUnidirectional: true},
			{Axis: linux.ABS_MT_SLOT, Min: 0, Value: 0, Max: maxSlots - 1},
			{Axis: linux.ABS_MT_POSITION_X, Min: 0, Value: 0, Max: axisMax, Resolution: resolution},
			{Axis: linux.ABS_MT_POSITION_Y, Min: 0, Value: 0, Max: axisMax, Resolution: resolution},
			{Axis: linux.ABS_MT_TRACKING_ID, Min: 0, Value: 0, Max: 65535},
		}).
		WithButtons([]linux.Button{
			// A real clickpad only exposes BTN_LEFT; BTN_RIGHT is software-emulated.
			linux.BTN_LEFT,
			linux.BTN_TOUCH,
			linux.BTN_TOOL_FINGER,
			linux.BTN_TOOL_DOUBLETAP,
			linux.BTN_TOOL_TRIPLETAP,
			linux.BTN_TOOL_QUADTAP,
			linux.BTN_TOOL_QUINTTAP,
		}).
		WithProperties([]linux.InputProp{
			linux.INPUT_PROP_POINTER,
			linux.INPUT_PROP_BUTTONPAD,
		}).
		Create()

	if err := tp.Register(); err != nil {
		return nil, fmt.Errorf("register virtual trackpad: %w", err)
	}

	d := &device{
		tp:     tp,
		active: make(map[int]touchpad.TouchSlot),
	}
	return d, nil
}

// Close unregisters the virtual device.
func (d *device) Close() error {
	return d.tp.Unregister()
}

// ProcessEvent applies a browser touch event to the virtual trackpad.
func (d *device) ProcessEvent(ev input.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	released := make(map[int]struct{})

	// Map pointer event names to the same lifecycle we used for touch events.
	var action string
	switch ev.Type {
	case "pointerdown":
		action = "down"
	case "pointermove":
		action = "move"
	case "pointerup", "pointercancel":
		action = "up"
	case "buttondown", "buttonup":
		// Button events are handled by the tablet device.
		return nil
	default:
		return fmt.Errorf("unknown touch/pointer event type: %q", ev.Type)
	}

	switch action {
	case "down", "move":
		for _, t := range ev.T {
			slot, ok := d.active[t.ID]
			if !ok {
				var err error
				slot, err = d.acquireSlot()
				if err != nil {
					return err
				}
			}
			slot.X = input.NormRaw(t.X, ev.W)
			slot.Y = input.NormRaw(t.Y, ev.H)
			if action == "down" || slot.Pressure == 0 {
				slot.Pressure = 1
			}
			d.active[t.ID] = slot
		}

	case "up":
		for _, t := range ev.T {
			if slot, ok := d.active[t.ID]; ok {
				slot.Pressure = 0
				d.active[t.ID] = slot
				released[t.ID] = struct{}{}
			}
		}
	}

	slots := make([]touchpad.TouchSlot, 0, len(d.active))
	for _, s := range d.active {
		slots = append(slots, s)
	}
	// Deterministic order so libinput sees a stable slot stream.
	sort.Slice(slots, func(i, j int) bool { return slots[i].Slot < slots[j].Slot })

	pressed := make([]*touchpad.TouchSlot, 0, len(slots))
	for i := range slots {
		if slots[i].Pressure > 0 {
			pressed = append(pressed, &slots[i])
		}
	}
	anyPressed := len(pressed) > 0

	// Emit BTN_TOUCH and single-touch axes only for the pointer phase. When
	// more than one finger is active we rely purely on MT events so the
	// desktop can classify two-finger scroll / pinch gestures.
	if anyPressed {
		if !d.touching {
			d.tp.Send(uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 1)
			d.touching = true
		}
		if len(pressed) == 1 {
			primary := pressed[0]
			d.tp.Send(uint16(linux.EV_ABS), uint16(linux.ABS_X), input.DenormBi(primary.X, 0, axisMax))
			d.tp.Send(uint16(linux.EV_ABS), uint16(linux.ABS_Y), input.DenormBi(primary.Y, 0, axisMax))
			d.tp.Send(uint16(linux.EV_ABS), uint16(linux.ABS_PRESSURE), input.DenormUni(primary.Pressure, 0, 255))
		}
	} else {
		if d.touching {
			d.tp.Send(uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 0)
			d.touching = false
		}
	}

	d.tp.MultiTouch(slots)

	// Free slots for released touches after the sync report.
	for id := range released {
		slot := d.active[id].Slot
		delete(d.active, id)
		d.releaseSlot(slot)
	}

	return nil
}

func (d *device) acquireSlot() (touchpad.TouchSlot, error) {
	for i := 0; i < maxSlots; i++ {
		if !d.slots[i] {
			d.slots[i] = true
			return touchpad.TouchSlot{Slot: i}, nil
		}
	}
	return touchpad.TouchSlot{}, fmt.Errorf("no free multitouch slots (max %d)", maxSlots)
}

func (d *device) releaseSlot(slot int) {
	if slot >= 0 && slot < maxSlots {
		d.slots[slot] = false
	}
}

// Reset releases all active contacts and leaves the trackpad idle, as if every
// finger had been lifted. It is intended to be called when a client
// disconnects so a subsequent client (or reconnect) does not inherit stuck
// contacts from a stroke whose pointerup was lost (e.g. a dropped data
// channel or a client that backgrounded the page mid-gesture).
func (d *device) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.active) == 0 && !d.touching {
		return
	}
	for id, slot := range d.active {
		slot.Pressure = 0
		d.active[id] = slot
	}
	if d.touching {
		d.tp.Send(uint16(linux.EV_KEY), uint16(linux.BTN_TOUCH), 0)
		d.touching = false
	}
	slots := make([]touchpad.TouchSlot, 0, len(d.active))
	for _, s := range d.active {
		slots = append(slots, s)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Slot < slots[j].Slot })
	d.tp.MultiTouch(slots)
	for id := range d.active {
		d.releaseSlot(d.active[id].Slot)
		delete(d.active, id)
	}
}
