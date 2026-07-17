// Package input defines the shared event protocol and coordinate helpers
// used by both the trackpad and graphics-tablet virtual input devices.
package input

// Touch is a single changed touch from the browser.
type Touch struct {
	ID int     `json:"id"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
}

// Event is the JSON payload sent over the WebRTC data channel.
type Event struct {
	Device string  `json:"device"`
	Type   string  `json:"type"`
	Button string  `json:"button,omitempty"`
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
