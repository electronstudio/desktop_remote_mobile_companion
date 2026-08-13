//go:build windows

package video

// EncoderLabels lists the --video-encoder choices offered by the GUI on
// Windows: the D3D11-capable hardware encoders (nvenc, amf, mf) plus the
// libx264 software encoder and auto. Hardware encoding is opt-in; auto
// resolves to libx264 (see resolveEncoder).
func EncoderLabels() []string {
	return []string{"nvenc", "amf", "mf", "libx264", "auto"}
}
