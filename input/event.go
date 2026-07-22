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
	ID       int      `json:"id"`
	X        float64  `json:"x"`
	Y        float64  `json:"y"`
	Pressure *float64 `json:"p,omitempty"`  // 0..1
	TiltX    *int     `json:"tx,omitempty"` // degrees, -90..90
	TiltY    *int     `json:"ty,omitempty"` // degrees, -90..90
}

// Event is the JSON payload sent over the WebRTC data channel.
type Event struct {
	Device string  `json:"device"`
	Type   string  `json:"type"`
	Button string  `json:"button,omitempty"`
	Active *bool   `json:"active,omitempty"` // control: tablet panel activation (type "activate")
	T      []Touch `json:"t"`
}

// Norm maps a browser [0,1] coordinate to the library's [-1,1] range.
func Norm(v float64) float32 {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return float32(2*v - 1)
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
