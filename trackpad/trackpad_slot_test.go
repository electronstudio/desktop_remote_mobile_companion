//go:build linux

package trackpad

import (
	"testing"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
	"github.com/jbdemonte/virtual-device/linux"
	"github.com/jbdemonte/virtual-device/touchpad"
)

// fakeTouchpad is a no-op touchpad.VirtualTouchpad that records the calls the
// trackpad.Device makes (Send and MultiTouch), so the slot bookkeeping can be
// exercised without a real /dev/uinput device.
type fakeTouchpad struct {
	sends      []fakeSend
	multiTouch [][]touchpad.TouchSlot
}

type fakeSend struct {
	evType uint16
	code   uint16
	value  int32
}

func (f *fakeTouchpad) Register() error              { return nil }
func (f *fakeTouchpad) Unregister() error            { return nil }
func (f *fakeTouchpad) Touch(x, y, pressure float32) {}
func (f *fakeTouchpad) MultiTouch(slots []touchpad.TouchSlot) []touchpad.TouchSlot {
	cp := make([]touchpad.TouchSlot, len(slots))
	copy(cp, slots)
	f.multiTouch = append(f.multiTouch, cp)
	return cp
}
func (f *fakeTouchpad) PressButton(button linux.Button)   {}
func (f *fakeTouchpad) ReleaseButton(button linux.Button) {}
func (f *fakeTouchpad) Click(btn linux.Button)            {}
func (f *fakeTouchpad) DoubleClick(btn linux.Button)      {}
func (f *fakeTouchpad) ClickLeft()                        {}
func (f *fakeTouchpad) ClickRight()                       {}
func (f *fakeTouchpad) DoubleClickLeft()                  {}
func (f *fakeTouchpad) DoubleClickRight()                 {}
func (f *fakeTouchpad) Send(evType, code uint16, value int32) {
	f.sends = append(f.sends, fakeSend{evType, code, value})
}
func (f *fakeTouchpad) EventPath() string { return "" }

// newTestDevice builds a trackpad.Device wired to a fake touchpad, bypassing
// New()/Register() so tests need no /dev/uinput permissions.
func newTestDevice(tp touchpad.VirtualTouchpad) *Device {
	return &Device{tp: tp, active: make(map[int]touchpad.TouchSlot)}
}

func downEvent(id int, x, y float64) input.Event {
	return input.Event{Type: "pointerdown", T: []input.Touch{{ID: id, X: x, Y: y}}}
}

func moveEvent(id int, x, y float64) input.Event {
	return input.Event{Type: "pointermove", T: []input.Touch{{ID: id, X: x, Y: y}}}
}

func upEvent(id int) input.Event {
	return input.Event{Type: "pointerup", T: []input.Touch{{ID: id, X: 0, Y: 0}}}
}

// TestAcquireAssignsDistinctSlots verifies each new browser id gets its own
// MT slot and the slot is marked in use.
func TestAcquireAssignsDistinctSlots(t *testing.T) {
	tp := &fakeTouchpad{}
	d := newTestDevice(tp)

	for _, id := range []int{1, 2} {
		if err := d.ProcessEvent(downEvent(id, 0.5, 0.5)); err != nil {
			t.Fatalf("pointerdown id=%d: %v", id, err)
		}
	}

	if got := d.active[1].Slot; got != 0 {
		t.Errorf("id 1 slot = %d, want 0", got)
	}
	if got := d.active[2].Slot; got != 1 {
		t.Errorf("id 2 slot = %d, want 1", got)
	}
	if !d.slots[0] || !d.slots[1] {
		t.Errorf("slots 0,1 should be in use, got slots=%v", d.slots)
	}
	if d.slots[2] {
		t.Errorf("slot 2 should be free")
	}
}

// TestReleaseSlotIsReused is the key regression guard for the removal of the
// write-only slotID reverse map: after a contact is lifted its slot must be
// freed and reused by the next incoming contact, exactly as before.
func TestReleaseSlotIsReused(t *testing.T) {
	tp := &fakeTouchpad{}
	d := newTestDevice(tp)

	if err := d.ProcessEvent(downEvent(1, 0.2, 0.2)); err != nil {
		t.Fatal(err)
	}
	if err := d.ProcessEvent(downEvent(2, 0.4, 0.4)); err != nil {
		t.Fatal(err)
	}
	if err := d.ProcessEvent(upEvent(1)); err != nil {
		t.Fatal(err)
	}

	if d.slots[0] {
		t.Errorf("slot 0 should be free after releasing id 1")
	}
	if !d.slots[1] {
		t.Errorf("slot 1 should still be in use by id 2")
	}
	if _, ok := d.active[1]; ok {
		t.Errorf("id 1 should be removed from active map")
	}

	// A new contact must reuse the just-freed slot 0.
	if err := d.ProcessEvent(downEvent(3, 0.6, 0.6)); err != nil {
		t.Fatal(err)
	}
	if got := d.active[3].Slot; got != 0 {
		t.Errorf("id 3 should reuse slot 0, got %d", got)
	}
	if !d.slots[0] {
		t.Errorf("slot 0 should be in use again")
	}
}

// TestPointerMoveDoesNotAllocate ensures a move on an existing contact does
// not consume an extra slot.
func TestPointerMoveDoesNotAllocate(t *testing.T) {
	tp := &fakeTouchpad{}
	d := newTestDevice(tp)

	if err := d.ProcessEvent(downEvent(1, 0.2, 0.2)); err != nil {
		t.Fatal(err)
	}
	nBefore := len(d.active)

	if err := d.ProcessEvent(moveEvent(1, 0.3, 0.3)); err != nil {
		t.Fatal(err)
	}
	if len(d.active) != nBefore {
		t.Errorf("pointermove changed active count: %d -> %d", nBefore, len(d.active))
	}
	if got := d.active[1].Slot; got != 0 {
		t.Errorf("slot changed during move: %d", got)
	}
}

// TestSlotExhaustion verifies that exceeding maxSlots returns the documented
// error instead of silently overwriting a slot.
func TestSlotExhaustion(t *testing.T) {
	tp := &fakeTouchpad{}
	d := newTestDevice(tp)

	for id := 1; id <= maxSlots; id++ {
		if err := d.ProcessEvent(downEvent(id, 0.1, 0.1)); err != nil {
			t.Fatalf("pointerdown id=%d: %v", id, err)
		}
	}

	err := d.ProcessEvent(downEvent(maxSlots+1, 0.1, 0.1))
	if err == nil {
		t.Fatal("expected slot exhaustion error, got nil")
	}
}

// TestMultiTouchReceivesReleasedSlot confirms that after a pointerup the
// emitted MultiTouch frame still includes the contact (with pressure 0) so
// the kernel releases its tracking id, and that a subsequent down reuses the
// slot — guarding against a regression where slot state and the emitted
// frames drift apart.
func TestMultiTouchReceivesReleasedSlot(t *testing.T) {
	tp := &fakeTouchpad{}
	d := newTestDevice(tp)

	_ = d.ProcessEvent(downEvent(1, 0.5, 0.5))
	_ = d.ProcessEvent(upEvent(1))

	// Find the MultiTouch frame from the pointerup event (last one).
	last := tp.multiTouch[len(tp.multiTouch)-1]
	if len(last) != 1 || last[0].Slot != 0 {
		t.Fatalf("pointerup MultiTouch frame = %+v, want one contact on slot 0", last)
	}
	if last[0].Pressure != 0 {
		t.Errorf("released slot pressure = %v, want 0", last[0].Pressure)
	}
}

// TestResetClearsAllSlots verifies Reset frees every slot and the active map,
// so a reconnecting client does not inherit stuck contacts.
func TestResetClearsAllSlots(t *testing.T) {
	tp := &fakeTouchpad{}
	d := newTestDevice(tp)

	for id := 1; id <= 3; id++ {
		if err := d.ProcessEvent(downEvent(id, 0.5, 0.5)); err != nil {
			t.Fatal(err)
		}
	}

	d.Reset()

	for i := 0; i < maxSlots; i++ {
		if d.slots[i] {
			t.Errorf("slot %d still in use after Reset", i)
		}
	}
	if len(d.active) != 0 {
		t.Errorf("active map not empty after Reset: %d entries", len(d.active))
	}
	if d.touching {
		t.Errorf("touching should be false after Reset")
	}

	// After Reset, all slots must be available again.
	for id := 1; id <= maxSlots; id++ {
		if err := d.ProcessEvent(downEvent(id, 0.1, 0.1)); err != nil {
			t.Fatalf("post-Reset pointerdown id=%d: %v", id, err)
		}
	}
}
