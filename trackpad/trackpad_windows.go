//go:build windows

// Package trackpad is a Windows stub. It implements the same Device API as the
// Linux uinput backend, but does not yet inject any input.
package trackpad

// TODO implement via SendInput / mouse_event
// uses SendInput mouse deltas for one-finger movement
// injects a second touch contact only for two-finger scroll?
// This requires custom gesture detection,

//
// Trackpad mode is the harder one. Windows only allows synthetic injection of PT_TOUCH or PT_PEN, not PT_TOUCHPAD. So the closest user-mode equivalent is injecting absolute multitouch points on the screen. Windows treats that as a touchscreen, so tap/scroll/pinch work, but the cursor does not move relatively the way a real trackpad does.
// A true Windows Precision Touchpad can only come from a real HID device (approach E). That is the only way Windows’ own gesture classification (two-finger scroll, pinch, three/ four-finger gestures) kicks in.

import (
	"github.com/electronstudio/desktop_remote_mobile_companion/input"
)

// Device is a placeholder Windows multitouch trackpad implementation.
type Device struct{}

// New creates a placeholder trackpad device.
func New() (*Device, error) {
	return &Device{}, nil
}

// Close is a no-op.
func (d *Device) Close() error { return nil }

// ProcessEvent ignores the event. Windows trackpad injection is not yet
// implemented.
func (d *Device) ProcessEvent(ev input.Event) error { return nil }
