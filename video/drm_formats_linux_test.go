//go:build linux

package video

import "testing"

// fourcc encodes four ASCII bytes little-endian, matching DRM's fourcc_code().
func fourcc(a, b, c, d byte) uint32 {
	return uint32(a) | uint32(b)<<8 | uint32(c)<<16 | uint32(d)<<24
}

// TestDRMFormatConstants pins the fourcc values against fourcc_code() so a typo
// in the hex literals is caught. The reported error case, 0x38344241, must be
// ABGR16161616.
func TestDRMFormatConstants(t *testing.T) {
	cases := map[string]uint32{
		"XRGB16161616": drmFormatXRGB16161616,
		"XBGR16161616": drmFormatXBGR16161616,
		"ARGB16161616": drmFormatARGB16161616,
		"ABGR16161616": drmFormatABGR16161616,
	}
	want := map[string][4]byte{
		"XRGB16161616": {'X', 'R', '4', '8'},
		"XBGR16161616": {'X', 'B', '4', '8'},
		"ARGB16161616": {'A', 'R', '4', '8'},
		"ABGR16161616": {'A', 'B', '4', '8'},
	}
	for name, got := range cases {
		c := want[name]
		if exp := fourcc(c[0], c[1], c[2], c[3]); got != exp {
			t.Errorf("%s = %#08x, want fourcc_code(%q) = %#08x", name, got, string(c[:]), exp)
		}
	}
	if drmFormatABGR16161616 != 0x38344241 {
		t.Errorf("ABGR16161616 = %#08x, want 0x38344241 (the reported error)", drmFormatABGR16161616)
	}
}

// TestDeepColorToPixFmt verifies the DRM fourcc -> software pixel format mapping
// and that unsupported formats are rejected. The two X/AR variants map to
// BGRA64LE, the two X/AB variants to RGBA64LE (little-endian byte order: the DRM
// msb->lsb channel order reversed into memory order).
func TestDeepColorToPixFmt(t *testing.T) {
	for _, f := range []uint32{drmFormatXRGB16161616, drmFormatARGB16161616} {
		pf, ok := deepColorToPixFmt(f)
		if !ok || pf.String() != "bgra64le" {
			t.Errorf("deepColorToPixFmt(%#08x) = %v,%v, want bgra64le,true", f, pf, ok)
		}
	}
	for _, f := range []uint32{drmFormatXBGR16161616, drmFormatABGR16161616} {
		pf, ok := deepColorToPixFmt(f)
		if !ok || pf.String() != "rgba64le" {
			t.Errorf("deepColorToPixFmt(%#08x) = %v,%v, want rgba64le,true", f, pf, ok)
		}
	}
	// An ordinary 8-bit format and an unmapped value must be rejected.
	for _, f := range []uint32{fourcc('X', 'R', '2', '4'), fourcc('N', 'V', '1', '2'), 0} {
		if _, ok := deepColorToPixFmt(f); ok {
			t.Errorf("deepColorToPixFmt(%#08x) should not be a deep-color format", f)
		}
	}
}

// TestPatchDRMFrameFormatNotDRM verifies the patcher is a safe no-op on a frame
// that is not AV_PIX_FMT_DRM_PRIME (e.g. a plain software frame): it must return
// 0 and not touch the frame.
func TestPatchDRMFrameFormatNotDRM(t *testing.T) {
	if got := patchDRMFrameFormat(nil, drmFormatABGR16161616, 0); got != 0 {
		t.Errorf("patchDRMFrameFormat(nil) = %#x, want 0", got)
	}
}
