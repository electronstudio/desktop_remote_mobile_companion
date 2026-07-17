package trackpad

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
)

// TestDevicePointerMotion is a manual test for single-finger pointer motion.
//
//	MANUAL_INPUT_TEST=1 go test ./trackpad -v -run TestDevicePointerMotion
func TestDevicePointerMotion(t *testing.T) {
	if os.Getenv("MANUAL_INPUT_TEST") != "1" {
		t.Skip("set MANUAL_INPUT_TEST=1 to run the manual input-event test")
	}

	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	fmt.Printf("EVENTPATH %s\n", d.tp.EventPath())
	time.Sleep(3 * time.Second)

	if err := d.ProcessEvent(input.Event{Type: "touchstart", T: []input.Touch{{ID: 1, X: 0.5, Y: 0.5}}}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		x := 0.5 + float64(i+1)*0.03
		if err := d.ProcessEvent(input.Event{Type: "touchmove", T: []input.Touch{{ID: 1, X: x, Y: 0.5}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := d.ProcessEvent(input.Event{Type: "touchend", T: []input.Touch{{ID: 1, X: 0.8, Y: 0.5}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1 * time.Second)
}

// TestDeviceEmitsEvents is a manual test. Run it while attaching evtest to the
// printed event path, e.g.:
//
//	MANUAL_INPUT_TEST=1 go test ./trackpad -v -run TestDeviceEmitsEvents
func TestDeviceEmitsEvents(t *testing.T) {
	if os.Getenv("MANUAL_INPUT_TEST") != "1" {
		t.Skip("set MANUAL_INPUT_TEST=1 to run the manual input-event test")
	}

	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	fmt.Printf("EVENTPATH %s\n", d.tp.EventPath())

	// Give time for a human observer to attach evtest to the printed path.
	time.Sleep(3 * time.Second)

	// Single-finger pointer motion: should include ABS_X/Y.
	if err := d.ProcessEvent(input.Event{Type: "touchstart", T: []input.Touch{{ID: 1, X: 0.5, Y: 0.5}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	if err := d.ProcessEvent(input.Event{Type: "touchmove", T: []input.Touch{{ID: 1, X: 0.6, Y: 0.6}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	// Second finger lands while the first is moving: ABS_X/Y should stop and
	// only MT events should be emitted, allowing libinput to start scrolling.
	if err := d.ProcessEvent(input.Event{Type: "touchstart", T: []input.Touch{{ID: 2, X: 0.4, Y: 0.6}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Scroll up: both fingers move upward (y decreases) several times.
	for i := 0; i < 5; i++ {
		y1 := 0.6 - float64(i+1)*0.05
		y2 := 0.6 - float64(i+1)*0.05
		if err := d.ProcessEvent(input.Event{Type: "touchmove", T: []input.Touch{
			{ID: 1, X: 0.65, Y: y1},
			{ID: 2, X: 0.45, Y: y2},
		}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	if err := d.ProcessEvent(input.Event{Type: "touchend", T: []input.Touch{{ID: 2, X: 0.45, Y: 0.35}, {ID: 1, X: 0.65, Y: 0.35}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1 * time.Second)
}
