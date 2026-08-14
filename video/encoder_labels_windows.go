//go:build windows

package video

import "fmt"

// EncoderLabels lists the --video-encoder choices offered by the GUI on
// Windows: the D3D11-capable hardware encoders (nvenc, amf, mf) plus the
// libx264 software encoder and auto. Auto resolves to mf (see
// resolveEncoder); nvenc and amf are opt-in.
func EncoderLabels() map[string]string {
	var labelMap = make(map[string]string)
	for _, e := range encoderKinds {
		if e.isWindows {
			description := fmt.Sprint(e.label, " (", e.description, ")")
			labelMap[description] = e.label
		}
	}
	return labelMap
}
