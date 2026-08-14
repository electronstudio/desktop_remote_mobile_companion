//go:build windows

package video

import "fmt"

// EncoderLabels lists the --video-encoder choices offered by the GUI on
// Windows: the D3D11-capable hardware encoders (nvenc, amf, mf) plus the
// libx264 software encoder and auto. Auto resolves to mf (see
// resolveEncoder); nvenc and amf are opt-in.
func EncoderLabels() map[string]string {
	var labels = make(map[string]string)
	for _, e := range encoderKinds {
		if e.isWindows {
			labels[e.label] = fmt.Sprint(e.label, " (", e.description, ")")
		}
	}
	return labels
}
