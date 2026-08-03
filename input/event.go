// Package input defines the shared event protocol and coordinate helpers
// used by both the trackpad and graphics-tablet virtual input devices.
package input

// Touch is a single changed touch from the browser.
//
// Pressure, TiltX and TiltY are optional pen/stylus attributes sourced
// from the browser PointerEvent. They are sent only for the tablet device
// and only when the browser actually reports them (pointers backed by a
// finger report a synthetic pressure of 0.5 and tilt of 0). Pointer (rather
// than value) types are used so "absent" is distinguishable from a real zero,
// which matters for pressure: 0 means "pen hovering, no contact".
type Touch struct {
	ID int `json:"id"`
	// X and Y are RAW panel-relative CSS-pixel coordinates (NOT normalised).
	// They are fractional on HiDPI screens and may lie outside [0,W]/[0,H]
	// when a captured pointer is dragged off the panel; the device code
	// normalises and clamps them against Event.W/Event.H.
	X        float64  `json:"x"`
	Y        float64  `json:"y"`
	Pressure *float64 `json:"p,omitempty"`  // 0..1
	TiltX    *int     `json:"tx,omitempty"` // degrees, -90..90
	TiltY    *int     `json:"ty,omitempty"` // degrees, -90..90
}

// Event is the JSON payload sent over the WebRTC data channel.
type Event struct {
	Device string `json:"device"`
	Type   string `json:"type"`
	Button string `json:"button,omitempty"`
	Active *bool  `json:"active,omitempty"` // control: tablet panel activation (type "activate")
	// W and H are the client panel's CSS-pixel size at send time. They are
	// fractional on HiDPI screens / fractional layouts. A zero (or negative)
	// value means "unknown"; device code must fall back to a neutral
	// coordinate rather than divide by it.
	W float64 `json:"w,omitempty"`
	H float64 `json:"h,omitempty"`
	T []Touch `json:"t"`
}

// NormRaw maps a raw panel coordinate (CSS px, 0..size) to the library's
// [-1,1] range, clamping out-of-panel values. A non-positive size (unknown
// panel dimensions) yields the neutral centre 0.
func NormRaw(v, size float64) float32 {
	if size <= 0 {
		return 0
	}
	n := v / size
	if n < 0 {
		n = 0
	}
	if n > 1 {
		n = 1
	}
	return float32(2*n - 1)
}

// DenormBi converts a [-1,1] value to an absolute axis range [min,max].
func DenormBi(v float32, min, max int32) int32 {
	if v < -1 {
		v = -1
	}
	if v > 1 {
		v = 1
	}
	return min + int32((v+1)*0.5*float32(max-min))
}

// DenormUni converts a [0,1] value to an absolute axis range [min,max].
func DenormUni(v float32, min, max int32) int32 {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return min + int32(v*float32(max-min))
}
