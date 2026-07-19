//go:build windows

// Package tablet is a Windows stub. It implements the same Device API as the
// Linux uinput backend, but does not yet inject any input.
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

// Device is a placeholder Windows graphics tablet implementation.
type Device struct{}

// New creates a placeholder graphics tablet device.
func New() (*Device, error) {
	return &Device{}, nil
}

// Close is a no-op.
func (d *Device) Close() error { return nil }

// ProcessEvent ignores the event. Windows pen/tablet injection is not yet
// implemented.
func (d *Device) ProcessEvent(ev input.Event) error { return nil }
