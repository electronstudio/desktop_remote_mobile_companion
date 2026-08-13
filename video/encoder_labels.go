//go:build !windows

package video

// EncoderLabels lists the --video-encoder choices offered by the GUI on this
// platform: the Linux hardware encoders plus the libx264 software encoder and
// auto. The Windows-only encoders (amf/mf) are listed in
// encoder_labels_windows.go instead.
func EncoderLabels() []string {
	return []string{"vaapi", "nvenc", "libx264", "auto"}
}
