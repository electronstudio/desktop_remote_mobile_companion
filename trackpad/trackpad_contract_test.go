package trackpad

import (
	"errors"
	"testing"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
)

// fakeDevice is a recording implementation of the trackpad.Device interface
// used to exercise the cross-platform contract (the lifecycle main.go relies
// on) without any platform backend.
type fakeDevice struct {
	processed []string
	reset     int
	closed    bool
}

func (f *fakeDevice) ProcessEvent(ev input.Event) error {
	f.processed = append(f.processed, ev.Type)
	if ev.Type == "boom" {
		return errors.New("boom")
	}
	return nil
}
func (f *fakeDevice) Reset() { f.reset++ }
func (f *fakeDevice) Close() error {
	f.closed = true
	return nil
}

// TestDeviceInterfaceContract asserts fakeDevice satisfies the Device
// interface at compile time and that the documented lifecycle (ProcessEvent ->
// Reset -> Close, and error propagation) works through the interface.
func TestDeviceInterfaceContract(t *testing.T) {
	var d Device = &fakeDevice{}

	if err := d.ProcessEvent(input.Event{Type: "pointerdown"}); err != nil {
		t.Fatalf("pointerdown through interface: %v", err)
	}
	if err := d.ProcessEvent(input.Event{Type: "boom"}); err == nil {
		t.Fatal("expected error to propagate through interface")
	}

	d.Reset()
	d.Reset()
	if got := d.(*fakeDevice).reset; got != 2 {
		t.Errorf("Reset count = %d, want 2", got)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close through interface: %v", err)
	}
	if !d.(*fakeDevice).closed {
		t.Error("Close did not mark fake closed")
	}
}

// TestNewReturnsDevice ensures the platform constructor returns a value
// satisfying the Device interface (not a concrete struct pointer). On Linux
// this registers a real uinput device, which needs /dev/uinput; on platforms
// without that it is a no-op stub. We only assert the type, not registration
// success, so this runs headless even without uinput permissions.
func TestNewReturnsDevice(t *testing.T) {
	d, err := New()
	if err != nil {
		// Registration may fail without /dev/uinput permissions; that is fine,
		// we only care that on success the returned type satisfies Device.
		return
	}
	defer d.Close()
	var _ Device = d
}
