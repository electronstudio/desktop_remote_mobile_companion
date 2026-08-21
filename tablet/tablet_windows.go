//go:build windows

// Package tablet is the Windows backend: it injects a synthetic pen (a
// "graphics tablet" pointer) into the Windows input pipeline using the
// user-mode injection API (CreateSyntheticPointerDevice +
// InjectSyntheticPointerInput, Windows 10 1809+). No driver or admin rights
// are required.
//
// The mapping mirrors the Linux uinput backend (tablet_linux.go):
// touching the phone's tablet panel is a pen-tip contact — the cursor
// follows and applications draw, with live pressure and tilt forwarded from
// the browser PointerEvent — and lifting the finger returns the pen to a
// hover while it stays in range between strokes, like a real pen. The
// injected frames are real pen events in the Windows Ink stack
// (WM_POINTER family, pressure 0..1024, tilt in degrees), so pressure- and
// tilt-aware applications (Krita/GIMP "Windows Ink", OneNote, etc.) work
// natively, and the pen cursor tracks the tablet without stealing the
// system mouse.
//
// Differences from the Linux backend: Windows has no libinput
// no-proximity-out quirk and no Mutter cooldown, so there is no keep-alive
// ticker, no ABS_PRESSURE tip threshold (contact is the INCONTACT/
// DOWN/UPDATE/UP pointer flags, not a pressure threshold) and therefore
// no tipFloor, and no proximity-in->tip-down delay. keepAlive is accepted
// for API parity and ignored.
//
// The Win32 structs (POINTER_TYPE_INFO carrying POINTER_PEN_INFO in its
// union), enums and the InjectSyntheticPointerInput /
// DestroySyntheticPointerDevice wrappers come from the metadata-generated
// github.com/deploymenttheory/go-bindings-win32 bindings. That module has
// no binding for CreateSyntheticPointerDevice (it is absent from Microsoft's
// win32metadata), so the one proc is resolved locally here. The PEN_MASK_*
// constants are #defines that likewise never reached the metadata and are
// defined locally.
package tablet

import (
	"fmt"
	"math"
	"sync"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/ui/input/pointer"
	uiwindowsandmessaging "github.com/deploymenttheory/go-bindings-win32/bindings/win32/ui/windowsandmessaging"
	"golang.org/x/sys/windows"

	"github.com/electronstudio/desktop_remote_mobile_companion/input"
	"github.com/electronstudio/desktop_remote_mobile_companion/video"
)

// Compile-time interface satisfaction check.
var _ Device = (*device)(nil)

const (
	// penPointerID identifies the single contact of the synthetic pen
	// device (created with maxContacts=1).
	penPointerID = 0

	// winPressureMax is the pointer API's pen pressure range. Note this is
	// a different (coarser) scale than the Linux ABS_PRESSURE axis
	// (0..4096); browser pressure 0..1 is rescaled at frame time.
	winPressureMax = 1024

	// PEN_MASK_* bits (winuser.h). Which pen properties a PEN frame
	// carries; unset properties are ignored by the receiver. Absent from
	// the win32metadata, so defined locally.
	penMaskPressure = 0x1
	penMaskRotation = 0x2
	penMaskTiltX    = 0x4
	penMaskTiltY    = 0x8

	// maxPenContacts is the contact count the synthetic device is created
	// with: a phone reports a single pen, so one.
	maxPenContacts = 1
)

// user32 procs missing from the metadata-generated bindings.
var (
	user32                           = windows.NewLazySystemDLL("user32.dll")
	procCreateSyntheticPointerDevice = user32.NewProc("CreateSyntheticPointerDevice")
)

// createSyntheticPointerDevice calls USER32!CreateSyntheticPointerDevice
// (Windows 10 1809+). It returns the handle of a synthetic pointer device of
// the given type with room for maxContacts simultaneous contacts, to be fed
// with InjectSyntheticPointerInput and released with
// DestroySyntheticPointerDevice.
// https://learn.microsoft.com/windows/win32/api/winuser/nf-winuser-createsyntheticpointerdevice
func createSyntheticPointerDevice(pointerType uiwindowsandmessaging.POINTER_INPUT_TYPE, maxContacts uint32, mode pointer.POINTER_FEEDBACK_MODE) (pointer.HSYNTHETICPOINTERDEVICE, error) {
	r, _, e := procCreateSyntheticPointerDevice.Call(
		uintptr(pointerType),
		uintptr(maxContacts),
		uintptr(mode))
	if r == 0 {
		if e != windows.ERROR_SUCCESS {
			return 0, fmt.Errorf("CreateSyntheticPointerDevice: %w", e)
		}
		return 0, fmt.Errorf("CreateSyntheticPointerDevice: returned NULL")
	}
	return pointer.HSYNTHETICPOINTERDEVICE(r), nil
}

// injector delivers one complete pen frame to the OS. It is a field on
// device so tests can record frames instead of injecting them (mirroring
// how the Linux backend's VirtualDevice is faked in tablet_linux_test.go).
type injector func(ti *pointer.POINTER_TYPE_INFO) error

// device is a virtual pen tablet (the Windows synthetic-pointer backend).
// It implements the tablet.Device interface.
type device struct {
	hdev     pointer.HSYNTHETICPOINTERDEVICE
	inj      injector
	mu       sync.Mutex
	inRange  bool
	tipDown  bool
	barrel1  bool // FIRSTBUTTON (right), held state
	barrel2  bool // SECONDBUTTON (middle), held state
	frameID  uint32
	nx, ny   float64 // content-normalised [0,1] position (see panelToContent)
	pressure float64 // browser pressure, clamped 0..1 (0 while hovering)
	tiltX    int32   // degrees, tiltMin..tiltMax
	tiltY    int32   // degrees, tiltMin..tiltMax
}

// newDevice creates a synthetic pen device and returns it as a Device. It
// fails with a clear error on Windows versions older than 1809 (the
// injection API does not exist), so the server can degrade gracefully.
// keepAlive is accepted for API parity with the Linux backend but has no
// effect: Windows has no equivalent of the libinput no-proximity-out quirk /
// Mutter cooldown the Linux keep-alive works around, and a pen hovering in
// silence between strokes requires no attention on Windows.
func newDevice(keepAlive bool) (Device, error) {
	if err := procCreateSyntheticPointerDevice.Find(); err != nil {
		return nil, fmt.Errorf("pen tablet needs Windows 10 1809 or later: %w", err)
	}
	hdev, err := createSyntheticPointerDevice(
		uiwindowsandmessaging.PT_PEN, maxPenContacts, pointer.POINTER_FEEDBACK_NONE)
	if err != nil {
		return nil, err
	}
	d := &device{hdev: hdev}
	d.inj = func(ti *pointer.POINTER_TYPE_INFO) error {
		return pointer.InjectSyntheticPointerInput(hdev, []pointer.POINTER_TYPE_INFO{*ti})
	}
	return d, nil
}

// newTestDevice builds a tablet.device wired to a recording injector,
// bypassing newDevice()/CreateSyntheticPointerDevice so tests need no
// injection API. It is the test-only constructor used by
// tablet_windows_test.go.
func newTestDevice(inj injector) *device {
	return &device{inj: inj}
}

// Close destroys the synthetic pointer device. The OS lifts any contact and
// forces the pen out of range when the device disappears, so no explicit
// proximity-out frame is needed.
func (d *device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.hdev != 0 {
		pointer.DestroySyntheticPointerDevice(d.hdev)
		d.hdev = 0
	}
	return nil
}

// ProcessEvent applies a browser event to the synthetic pen. It mirrors the
// Linux backend's mapping: pointerdown = proximity-in + tip-down,
// pointermove = contact/hover update, pointerup = tip-up (the pen stays in
// range, hovering), buttondown/up = tip (left) or barrel (right/middle)
// buttons.
func (d *device) ProcessEvent(ev input.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch ev.Type {
	case "pointerdown":
		if len(ev.T) == 0 {
			return nil
		}
		d.setPen(ev.T[0], ev.W, ev.H, video.CaptureWidth.Load(), video.CaptureHeight.Load())
		return d.beginStroke()
	case "pointermove":
		// Moved while tipping: contact update with live pressure/tilt. Moved
		// while hovering: hover frame (pressure 0). A first move before any
		// pointerdown is treated as a stroke start, as on Linux.
		if len(ev.T) == 0 {
			return nil
		}
		d.setPen(ev.T[0], ev.W, ev.H, video.CaptureWidth.Load(), video.CaptureHeight.Load())
		if !d.inRange {
			if err := d.proximityIn(); err != nil {
				return err
			}
			return d.dropTip()
		}
		if d.tipDown {
			return d.emitFrame(pointer.POINTER_FLAG_UPDATE|pointer.POINTER_FLAG_INRANGE|pointer.POINTER_FLAG_INCONTACT, d.pressure)
		}
		return d.emitFrame(pointer.POINTER_FLAG_UPDATE|pointer.POINTER_FLAG_INRANGE, 0)
	case "pointerup", "pointercancel":
		// Pen lifts off the surface: release the tip but stay in range,
		// hovering — a pen keeps hovering between strokes (a phone has no
		// real hover, but staying in range needs no attention on Windows,
		// unlike Linux; see the package comment). The UP frame carries the
		// final position, so no extra hover frame is needed.
		d.barrel1, d.barrel2 = false, false
		if d.inRange && d.tipDown {
			return d.liftTip()
		}
	case "buttondown", "buttonup":
		pressed := ev.Type == "buttondown"
		switch ev.Button {
		case "left":
			// Explicit tip contact (click without a touch/draw), sharing the
			// tip with a real pen contact; the two are not used together.
			if !d.inRange {
				if err := d.proximityIn(); err != nil {
					return err
				}
			}
			if pressed {
				return d.dropTip()
			}
			return d.liftTip()
		case "right", "middle":
			if ev.Button == "right" {
				d.barrel1 = pressed
			} else {
				d.barrel2 = pressed
			}
			// Report the barrel-button change. Held barrel buttons ride on
			// every subsequent frame via baseFlags.
			if d.tipDown {
				return d.emitFrame(pointer.POINTER_FLAG_UPDATE|pointer.POINTER_FLAG_INRANGE|pointer.POINTER_FLAG_INCONTACT, d.pressure)
			}
			if !d.inRange {
				return d.proximityIn()
			}
			return d.emitFrame(pointer.POINTER_FLAG_UPDATE|pointer.POINTER_FLAG_INRANGE, 0)
		default:
			return fmt.Errorf("unknown button: %q", ev.Button)
		}
	default:
		return fmt.Errorf("unknown event type: %q", ev.Type)
	}
	return nil
}

// Reset releases the tool when a client disconnects or the tablet panel
// deactivates: lift the tip and leave proximity (a final frame with neither
// INRANGE nor INCONTACT). Injection errors are swallowed: there is nothing
// to return them to, and the device state must end clean regardless.
func (d *device) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.inRange {
		return
	}
	if d.tipDown {
		_ = d.liftTip()
	}
	d.barrel1, d.barrel2 = false, false
	_ = d.emitFrame(pointer.POINTER_FLAG_UPDATE, 0)
	d.inRange = false
}

// SetActive is the control handler for the client's "activate" message.
// When the tablet panel is swiped away (active=false) the tool is released
// (proximity-out) so no stale pen state lingers while the user is not
// drawing. On true nothing is done — the next touch does a fresh
// proximity-in.
func (d *device) SetActive(active bool) {
	if !active {
		d.Reset()
	}
}

// beginStroke brings the pen into contact. Like the Linux backend it first
// recovers from stale state (a previous stroke whose tip-up was lost, e.g.
// the client disconnected mid-stroke): if the pen still appears down it is
// lifted so the OS sees a real down transition for the new stroke.
func (d *device) beginStroke() error {
	if d.inRange && d.tipDown {
		if err := d.liftTip(); err != nil {
			return err
		}
	}
	if !d.inRange {
		if err := d.proximityIn(); err != nil {
			return err
		}
	}
	return d.dropTip()
}

// proximityIn brings the pen into range as a new pointer: NEW | INRANGE,
// pressure 0. The subsequent tip (if any) arrives in a separate frame.
func (d *device) proximityIn() error {
	if err := d.emitFrame(pointer.POINTER_FLAG_NEW|pointer.POINTER_FLAG_UPDATE|pointer.POINTER_FLAG_INRANGE, 0); err != nil {
		return err
	}
	d.inRange = true
	return nil
}

// dropTip drops the pen tip onto the surface: DOWN | INCONTACT with the
// pen's current pressure.
func (d *device) dropTip() error {
	if d.tipDown {
		return nil
	}
	if err := d.emitFrame(pointer.POINTER_FLAG_DOWN|pointer.POINTER_FLAG_INRANGE|pointer.POINTER_FLAG_INCONTACT, d.pressure); err != nil {
		return err
	}
	d.tipDown = true
	return nil
}

// liftTip lifts the pen tip off the surface: UP, back to hover pressure.
// The pen stays in range (see ProcessEvent "pointerup").
func (d *device) liftTip() error {
	if !d.tipDown {
		return nil
	}
	if err := d.emitFrame(pointer.POINTER_FLAG_UP|pointer.POINTER_FLAG_INRANGE, 0); err != nil {
		return err
	}
	d.tipDown = false
	return nil
}

// baseFlags returns the bits present on every frame this device emits:
// PRIMARY (this is the session's primary pointer) plus the barrel-button
// state. WINDOWS derives button presses from these flags; a button press or
// release is just a frame with the flag added or removed (see "buttondown").
func (d *device) baseFlags() pointer.POINTER_FLAGS {
	f := pointer.POINTER_FLAG_PRIMARY
	if d.barrel1 {
		f |= pointer.POINTER_FLAG_FIRSTBUTTON
	}
	if d.barrel2 {
		f |= pointer.POINTER_FLAG_SECONDBUTTON
	}
	return f
}

// emitFrame renders the cached pen state as one POINTER_TYPE_INFO pen frame
// and injects it. flags carries the transition/state bits
// (NEW/INRANGE/INCONTACT/DOWN/UPDATE/UP); PRIMARY and the barrel buttons are
// added automatically. pressure is the browser's 0..1 value for contact
// frames and 0 for hover frames — held barrel buttons ride on the frame but
// a hovering pen always reports pressure 0, like a real one.
func (d *device) emitFrame(flags pointer.POINTER_FLAGS, pressure float64) error {
	var ti pointer.POINTER_TYPE_INFO
	ti.Type = uiwindowsandmessaging.PT_PEN
	pi := (*pointer.POINTER_PEN_INFO)(unsafe.Pointer(&ti.Anonymous))

	// Map the content-normalised position onto the virtual screen
	// (multi-monitor) origin+extent. The metrics are read per frame so a
	// monitor hot-plug is picked up instead of offsetting the pen into the
	// void; (cx-1, cy-1) maps 1.0 to the last pixel.
	vx := uiwindowsandmessaging.GetSystemMetrics(uiwindowsandmessaging.SM_XVIRTUALSCREEN)
	vy := uiwindowsandmessaging.GetSystemMetrics(uiwindowsandmessaging.SM_YVIRTUALSCREEN)
	cx := uiwindowsandmessaging.GetSystemMetrics(uiwindowsandmessaging.SM_CXVIRTUALSCREEN) - 1
	cy := uiwindowsandmessaging.GetSystemMetrics(uiwindowsandmessaging.SM_CYVIRTUALSCREEN) - 1

	pi.PointerInfo.PointerType = uiwindowsandmessaging.PT_PEN
	pi.PointerInfo.PointerId = penPointerID
	pi.PointerInfo.FrameId = d.frameID
	pi.PointerInfo.PointerFlags = d.baseFlags() | flags
	pi.PointerInfo.PtPixelLocation = foundation.POINT{
		X: vx + int32(math.Min(d.nx, 1)*float64(cx)),
		Y: vy + int32(math.Min(d.ny, 1)*float64(cy)),
	}
	pi.PenMask = penMaskPressure | penMaskTiltX | penMaskTiltY
	pi.Pressure = uint32(pressure * winPressureMax)
	pi.TiltX = d.tiltX
	pi.TiltY = d.tiltY

	if err := d.inj(&ti); err != nil {
		return fmt.Errorf("inject pen frame: %w", err)
	}
	d.frameID++
	return nil
}

// setPen updates the cached pen state (position, pressure, tilt) from a
// browser touch sample. x/y are raw panel CSS-pixel coordinates; w/h are the
// panel size; capW/capH are the desktop video's capture resolution (0 = no
// active stream): the client renders the video with object-fit: contain, so
// panelToContent remaps the pen onto the visible (letterboxed) image and the
// pen tracks the desktop picture. Absent pressure is 0 (hover); absent tilt
// is 0 degrees. See the Linux setPen, whose semantics this mirrors.
func (d *device) setPen(t input.Touch, w, h float64, capW, capH int64) {
	d.nx, d.ny = panelToContent(t.X, t.Y, w, h, float64(capW), float64(capH))
	if t.Pressure != nil {
		d.pressure = clamp(*t.Pressure)
	} else {
		d.pressure = 0
	}
	d.tiltX = clampTilt(t.TiltX)
	d.tiltY = clampTilt(t.TiltY)
}
