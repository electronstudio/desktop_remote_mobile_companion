// Package tablet creates a virtual graphics tablet and maps browser touch
// events to absolute screen coordinates.
package tablet

import (
	"math"

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

const (
	tiltMin = -90 // pen tilt range, degrees (matches PointerEvent tiltX/tiltY)
	tiltMax = 90
)

// panelToContent maps a raw panel coordinate (CSS px) to the fraction
// [0,1] of the desktop it points at, accounting for the black bars the
// client adds with object-fit: contain when the desktop video's aspect ratio
// (capW x capH) differs from the panel's (panelW x panelH). The video is
// scaled by s = min(panelW/capW, panelH/capH) and centred, so the displayed
// image occupies disp = cap*s on each axis, offset by off = (panel-disp)/2;
// coordinates in the bars clamp to the desktop edge. Coordinates outside
// the panel (a captured pointer dragged off the panel) clamp to the panel first.
//
// With no active video (capW/capH <= 0) the mapping is the identity v/panel
// so the tablet still spans the whole desktop (--video-source=none). A
// non-positive panel extent (unknown dimensions) parks that axis at its
// centre rather than dividing by zero.
//
// Shared by the Linux and Windows backends: each consumes the result
// differently (evdev axis units vs virtual-screen pixels) but the
// panel->desktop mapping is identical.
func panelToContent(x, y, panelW, panelH, capW, capH float64) (float64, float64) {
	nx := normClamp(x, panelW)
	ny := normClamp(y, panelH)
	if capW <= 0 || capH <= 0 || panelW <= 0 || panelH <= 0 {
		return nx, ny
	}
	scale := math.Min(panelW/capW, panelH/capH)
	dispW := capW * scale
	dispH := capH * scale
	offX := (panelW - dispW) / 2
	offY := (panelH - dispH) / 2
	cx := clamp((x - offX) / dispW)
	cy := clamp((y - offY) / dispH)
	return cx, cy
}

// normClamp divides v by size and clamps the result to [0,1], clamping v to
// the panel first so a captured pointer dragged off the panel pins to its
// edge. A non-positive size (unknown panel dimensions) yields the neutral
// centre 0.5.
func normClamp(v, size float64) float64 {
	if size <= 0 {
		return 0.5
	}
	if v < 0 {
		v = 0
	}
	if v > size {
		v = size
	}
	return v / size
}

// clamp constrains v to [0,1].
func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// clampTilt converts the browser's optional tilt (*int, "absent" is
// distinguishable from 0) to a clamped axis value in degrees.
func clampTilt(v *int) int32 {
	if v == nil {
		return 0
	}
	t := int32(*v)
	if t < tiltMin {
		return tiltMin
	}
	if t > tiltMax {
		return tiltMax
	}
	return t
}
