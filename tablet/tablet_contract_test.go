package tablet

import (
	"testing"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
)

// fakeDevice is a recording implementation of the tablet.Device interface
// used to exercise the cross-platform contract (the lifecycle main.go relies
// on) without any platform backend.
type fakeDevice struct {
	processed []string
	setActive []bool
	reset     int
	closed    bool
}

func (f *fakeDevice) ProcessEvent(ev input.Event) error {
	f.processed = append(f.processed, ev.Type)
	return nil
}
func (f *fakeDevice) SetActive(a bool) { f.setActive = append(f.setActive, a) }
func (f *fakeDevice) Reset()           { f.reset++ }
func (f *fakeDevice) Close() error     { f.closed = true; return nil }

// TestDeviceInterfaceContract asserts fakeDevice satisfies the Device
// interface at compile time and that the documented lifecycle (ProcessEvent ->
// SetActive -> Reset -> Close) works through the interface.
func TestDeviceInterfaceContract(t *testing.T) {
	var d Device = &fakeDevice{}

	if err := d.ProcessEvent(input.Event{Type: "pointerdown"}); err != nil {
		t.Fatalf("pointerdown through interface: %v", err)
	}
	d.SetActive(true)
	d.SetActive(false)
	if got := d.(*fakeDevice).setActive; len(got) != 2 || !got[0] || got[1] {
		t.Errorf("SetActive calls = %v, want [true false]", got)
	}

	d.Reset()
	if got := d.(*fakeDevice).reset; got != 1 {
		t.Errorf("Reset count = %d, want 1", got)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close through interface: %v", err)
	}
	if !d.(*fakeDevice).closed {
		t.Error("Close did not mark fake closed")
	}
}
