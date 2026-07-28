//go:build linux

package video

import (
	"testing"

	"github.com/asticode/go-astiav"
)

// TestParseFilterGraphWithDevice_VaapiHwupload reproduces the x11grab + vaapi
// path: a software buffer source feeding "format=nv12,hwupload=derive_device=
// vaapi" into a buffersink, with a VAAPI hardware device context attached via
// parseFilterGraphWithDevice. Before the fix this failed during parse with
// "A hardware device reference is required to upload frames to." because
// go-astiav's FilterGraph.Parse initializes hwupload before its hw_device_ctx
// is set. This test needs a VAAPI-capable GPU; it skips if the device cannot be
// created.
func TestParseFilterGraphWithDevice_VaapiHwupload(t *testing.T) {
	hwdev, err := astiav.CreateHardwareDeviceContext(astiav.HardwareDeviceTypeVAAPI, "", nil, 0)
	if err != nil {
		t.Skipf("cannot create VAAPI hardware device context: %v", err)
	}
	defer hwdev.Free()

	g := astiav.AllocFilterGraph()
	if g == nil {
		t.Fatal("alloc filter graph")
	}
	defer g.Free()

	buffersrc := astiav.FindFilterByName("buffer")
	buffersink := astiav.FindFilterByName("buffersink")
	if buffersrc == nil || buffersink == nil {
		t.Fatal("buffer/buffersink filter not found")
	}

	bsrc, err := g.NewBuffersrcFilterContext(buffersrc, "in")
	if err != nil {
		t.Fatalf("new buffersrc: %v", err)
	}
	bsink, err := g.NewBuffersinkFilterContext(buffersink, "out")
	if err != nil {
		t.Fatalf("new buffersink: %v", err)
	}

	params := astiav.AllocBuffersrcFilterContextParameters()
	defer params.Free()
	params.SetWidth(320)
	params.SetHeight(240)
	params.SetPixelFormat(astiav.PixelFormatYuv420P)
	params.SetTimeBase(astiav.NewRational(1, 30))
	if err := bsrc.SetParameters(params); err != nil {
		t.Fatalf("set buffersrc params: %v", err)
	}
	if err := bsrc.Initialize(nil); err != nil {
		t.Fatalf("init buffersrc: %v", err)
	}

	outputs := astiav.AllocFilterInOut()
	defer outputs.Free()
	inputs := astiav.AllocFilterInOut()
	defer inputs.Free()
	outputs.SetName("in")
	outputs.SetFilterContext(bsrc.FilterContext())
	outputs.SetPadIdx(0)
	outputs.SetNext(nil)
	inputs.SetName("out")
	inputs.SetFilterContext(bsink.FilterContext())
	inputs.SetPadIdx(0)
	inputs.SetNext(nil)

	desc := "format=nv12,hwupload=derive_device=vaapi"
	if err := parseFilterGraphWithDevice(g, desc, inputs, outputs, hwdev); err != nil {
		t.Fatalf("parseFilterGraphWithDevice: %v", err)
	}
	if err := g.Configure(); err != nil {
		t.Fatalf("configure filter graph: %v", err)
	}
}
