//go:build windows

// Package tablet is a Windows stub. It implements the same Device interface
// as the Linux uinput backend, but does not yet inject any input.
package tablet

// TODO: Implement Windows tablet via synthetic pen injection
//- CreateSyntheticPointerDevice(PT_PEN, 1, POINTER_FEEDBACK_NONE).
// InjectSyntheticPointerInput
//- Maintain one pen contact (the tablet is single-point).
//- Map pointerdown/move/up to hover/in-contact/up frames.
//- Map buttondown/up for left/right/middle to tip/barrel/second-barrel states.

//  Tablet mode maps very cleanly to the synthetic-pointer pen API (PT_PEN):
//- phone touch = pen hover (INRANGE, pressure 0)
//- L button = pen tip down (INCONTACT, pressure > 0)
//- R/M buttons = barrel / second-barrel buttons (PEN_FLAG_BARREL, POINTER_FLAG_SECONDBUTTON / THIRDBUTTON)
//- absolute X/Y mapped to virtual-screen pixels.

// There is a ready-made Go binding package, github.com/deploymenttheory/go-bindings-win32/bindings/win32/ui/input/pointer, that already exposes InjectSyntheticPointerInput, the POINTER_PEN_INFO / POINTER_TOUCH_INFO structs, and pointer flags. It is missing CreateSyntheticPointerDevice, but that single function is easy to bind by hand with syscall.NewLazyDLL("user32.dll").

import (
	"github.com/electronstudio/desktop_remote_mobile_companion/input"
)

// Compile-time interface satisfaction check.
var _ Device = (*device)(nil)

// device is a placeholder Windows graphics tablet implementation. It
// implements the tablet.Device interface but injects nothing.
type device struct{}

// newDevice creates a placeholder graphics tablet device (Windows backend)
// and returns it as a Device. keepAlive is accepted for API parity with the
// Linux backend but has no effect. It is the platform-specific constructor
// called by the cross-platform New in tablet.go.
func newDevice(keepAlive bool) (Device, error) {
	return &device{}, nil
}

// Close is a no-op.
func (d *device) Close() error { return nil }

// ProcessEvent ignores the event. Windows pen/tablet injection is not yet
// implemented.
func (d *device) ProcessEvent(ev input.Event) error { return nil }

// SetActive is a no-op; there is no tool state to release on Windows.
func (d *device) SetActive(active bool) {}

// Reset is a no-op; there is no tool state to release on Windows.
func (d *device) Reset() {}
