//go:build !windows

package video

import "fmt"

// EncoderLabels lists the --video-encoder choices offered by the GUI on this
// platform: the Linux hardware encoders plus the libx264 software encoder and
// auto. The Windows-only encoders (amf/mf) are listed in
// encoder_labels_windows.go instead.
func EncoderLabels() map[string]string {
	var labelMap = make(map[string]string)
	for _, e := range encoderKinds {
		if e.isLinux {
			description := fmt.Sprint(e.label, " (", e.description, ")")
			labelMap[description] = e.label
		}
	}
	return labelMap
}
