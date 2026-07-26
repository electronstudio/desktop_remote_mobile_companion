// Package trackpad creates a virtual multitouch trackpad and maps browser touch
// events to native input events.
package trackpad

import (
	"github.com/electronstudio/desktop_remote_mobile_companion/input"
)

// Device is a virtual multitouch trackpad. It receives browser pointer events
// and maps them to native trackpad input.
type Device interface {
	// ProcessEvent applies a browser touch/pointer event to the trackpad.
	ProcessEvent(ev input.Event) error
	// Reset releases all active contacts and leaves the trackpad idle, as if
	// every finger had been lifted. Intended for client disconnect so a
	// reconnecting client does not inherit stuck contacts.
	Reset()
	// Close unregisters the virtual device and releases any resources.
	Close() error
}

// New creates and registers a virtual multitouch trackpad for the current
// platform. On platforms without a real implementation it returns a no-op
// device.
func New() (Device, error) {
	return newDevice()
}
