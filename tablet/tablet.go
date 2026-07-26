// Package tablet creates a virtual graphics tablet and maps browser touch
// events to absolute screen coordinates.
package tablet

import (
	"github.com/electronstudio/desktop_remote_mobile_companion/input"
)

// Device is a virtual graphics tablet. It receives browser pointer and button
// events and maps them to native tablet/pen input.
type Device interface {
	// ProcessEvent applies a browser pointer event (pointerdown/move/up/cancel
	// or buttondown/up) to the tablet.
	ProcessEvent(ev input.Event) error
	// SetActive is the control handler for the client's "activate" message.
	// When the tablet panel becomes the active panel the client sends
	// active=true; when it is swiped away it sends active=false, which releases
	// the tool (proximity-out) so the system mouse works again.
	SetActive(active bool)
	// Reset releases the tool when a client disconnects: lift the tip and leave
	// proximity cleanly. Intended for client disconnect so a reconnecting
	// client does not inherit a stuck tip.
	Reset()
	// Close unregisters the virtual device and releases any resources.
	Close() error
}

// New creates and registers a virtual graphics tablet for the current
// platform. keepAlive controls the hover keep-alive workaround (see the
// Linux backend). On platforms without a real implementation it returns a
// no-op device.
func New(keepAlive bool) (Device, error) {
	return newDevice(keepAlive)
}
