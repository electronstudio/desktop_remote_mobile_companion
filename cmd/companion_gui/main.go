package main

// companion_gui is a Fyne front-end for the server. Build with the
// `migrated_fynedo` tag so Fyne does not log its threading-model migration
// warning at startup:
//
//     go build -tags migrated_fynedo -o companion_gui ./cmd/companion_gui
//
// This is safe because the server (started by the Start button) runs in its
// own goroutine and never calls any Fyne API; all Fyne calls happen on the
// main/app goroutine.
//
// Everything written through the standard log package (log.Printf etc.) from
// any package (server, signaling, video, ...) is mirrored into the read-only
// text area at the bottom of the window, in addition to the terminal.

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/electronstudio/desktop_remote_mobile_companion/server"
)

// maxLogLines caps how many log lines the text area retains so it does not
// grow without bound over a long session.
const maxLogLines = 5000

// logView is an io.Writer that captures lines written via the standard log
// package and mirrors them into a Fyne text widget. It is safe for
// concurrent use: log output comes from many goroutines (HTTP handlers,
// WebRTC callbacks, the FFmpeg capture loop).
type logView struct {
	mu       sync.Mutex
	pending  string   // partial line not yet terminated by '\n'
	lines    []string // retained complete lines, capped at maxLogLines
	label    *widget.Label
	scroll   *container.Scroll
	notifyCh chan struct{} // capacity 1; coalesces bursts of log writes
}

func newLogView() *logView {
	l := widget.NewLabel("")
	l.Wrapping = fyne.TextWrapWord
	l.Selectable = true // allows selecting/copying log text

	lv := &logView{
		label:    l,
		scroll:   container.NewVScroll(l),
		notifyCh: make(chan struct{}, 1),
	}
	return lv
}

// Write implements io.Writer. The stdlib log package writes one line per
// call, but we buffer on '\n' anyway so partial writes cannot corrupt the
// display.
func (lv *logView) Write(p []byte) (int, error) {
	lv.mu.Lock()
	lv.pending += string(p)
	for {
		i := strings.IndexByte(lv.pending, '\n')
		if i < 0 {
			break
		}
		lv.lines = append(lv.lines, lv.pending[:i])
		lv.pending = lv.pending[i+1:]
	}
	if overflow := len(lv.lines) - maxLogLines; overflow > 0 {
		lv.lines = append([]string(nil), lv.lines[overflow:]...)
	}
	lv.mu.Unlock()

	// Non-blocking notify: if a flush is already pending, this burst will be
	// picked up by it.
	select {
	case lv.notifyCh <- struct{}{}:
	default:
	}
	return len(p), nil
}

// flushLoop runs in its own goroutine for the lifetime of the app. It waits
// for log activity, coalesces bursts, and applies the accumulated text to
// the widget on the Fyne main thread via fyne.Do.
func (lv *logView) flushLoop() {
	for range lv.notifyCh {
		lv.mu.Lock()
		text := strings.Join(lv.lines, "\n")
		lv.mu.Unlock()

		fyne.Do(func() {
			lv.label.SetText(text)
			lv.scroll.ScrollToBottom()
		})
	}
}

// driDevices returns the names of the device nodes in /dev/dri (e.g.
// "card1", "renderD128"), sorted, with the empty string prepended as the
// first option, meaning auto-detect. On any error (directory missing, e.g.
// on Windows) it returns just the auto-detect option so the GUI still works.
func driDevices() []string {
	names := []string{""}
	entries, err := os.ReadDir("/dev/dri")
	if err != nil {
		return names
	}
	for _, e := range entries {
		if e.IsDir() { // skip by-path/
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names[1:])
	return names
}

func main() {
	// Default server configuration, taken from the go-arg `default:` struct
	// tags on server.CLI so the GUI starts with the same configuration as
	// `companion` with no arguments. The GUI ignores command-line flags; the
	// widgets below edit this struct before the Start button passes it to
	// server.Run.
	cli := server.CLIDefaults()
	cli.DontRunSudo = true

	// NewWithID gives the app a stable unique ID (required by the Preferences
	// API and for a stable per-user config/cache directory). The app must be
	// created before any widget: creating/refreshing a widget looks up the
	// current app and panics if none exists yet.
	a := app.NewWithID("co.electronstudio.desktop_remote_mobile_companion.gui")
	w := a.NewWindow("Desktop Remote Mobile Companion")

	// Mirror all standard log output (from any package) into the GUI text
	// area while keeping it on the terminal too.
	logs := newLogView()
	log.SetOutput(io.MultiWriter(os.Stderr, logs))
	go logs.flushLoop()

	// Configuration widgets. Each one writes the edited value into cli, which
	// the Start button captures when it launches the server.
	portEntry := widget.NewEntry()
	portEntry.SetText(strconv.Itoa(cli.Port))
	portEntry.OnChanged = func(s string) {
		if port, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			cli.Port = port
		}
		// Invalid input is ignored: cli keeps the last valid port, so a
		// half-typed value cannot break the server start.
	}

	videoSourceSelect := widget.NewSelect([]string{"kmsgrab", "x11grab", "none"}, func(s string) {
		cli.VideoSource = s
	})
	videoSourceSelect.SetSelected(cli.VideoSource)

	videoEncoderSelect := widget.NewSelect([]string{"vaapi", "nvenc", "libx264", "auto"}, func(s string) {
		cli.VideoEncoder = s
	})
	if cli.VideoEncoder == "" {
		cli.VideoEncoder = "auto" // "" is accepted by the server but display the canonical name
	}
	videoEncoderSelect.SetSelected(cli.VideoEncoder)

	// Video card selector: every device node in /dev/dri, enumerated once at
	// startup. The first option is auto-detect, stored as "" in cli.VideoCard
	// (an empty string would render as a blank dropdown row, so it is
	// displayed as "auto-detect" instead).
	const videoCardAuto = "auto-detect"
	cardOptions := make([]string, 0, 8)
	for _, name := range driDevices() {
		if name == "" {
			cardOptions = append(cardOptions, videoCardAuto)
		} else {
			cardOptions = append(cardOptions, name)
		}
	}
	videoCardSelect := widget.NewSelect(cardOptions, func(s string) {
		if s == videoCardAuto {
			cli.VideoCard = ""
		} else {
			cli.VideoCard = "/dev/dri/" + s
		}
	})
	videoCardSelect.SetSelected(videoCardAuto)

	// Video FPS entry: like the port entry, invalid or non-positive input is
	// silently ignored so a half-typed value cannot break the server start.
	videoFpsEntry := widget.NewEntry()
	videoFpsEntry.SetText(strconv.Itoa(cli.VideoFps))
	videoFpsEntry.OnChanged = func(s string) {
		if fps, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && fps > 0 {
			cli.VideoFps = fps
		}
	}

	intelFastCheck := widget.NewCheck("Intel GPU fast mode", func(on bool) {
		cli.VideoIntelFast = on
	})
	intelFastCheck.SetChecked(cli.VideoIntelFast)

	dontGrabCheck := widget.NewCheck("Don't grab mouse", func(on bool) {
		cli.DontGrabMouse = on
	})
	dontGrabCheck.SetChecked(cli.DontGrabMouse)

	form := widget.NewForm(
		widget.NewFormItem("Port", portEntry),
		widget.NewFormItem("Video source", videoSourceSelect),
		widget.NewFormItem("Video encoder", videoEncoderSelect),
		widget.NewFormItem("Video card", videoCardSelect),
		widget.NewFormItem("Video FPS", videoFpsEntry),
	)

	start := widget.NewButton("Start", nil)
	start.OnTapped = func() {
		start.Disable()
		start.SetText("Running…")
		// Disable the config widgets: the running server has already captured
		// cli, so further edits would have no effect and suggest otherwise.
		portEntry.Disable()
		videoSourceSelect.Disable()
		videoEncoderSelect.Disable()
		videoCardSelect.Disable()
		videoFpsEntry.Disable()
		intelFastCheck.Disable()
		dontGrabCheck.Disable()
		// Run the server in the background so the window stays responsive.
		// The server blocks forever (ListenAndServeTLS); it has no stop path,
		// so closing the window exits the whole process (see SetOnClosed).
		go server.Run(cli)
	}

	fix_permissions := widget.NewButton("Fix permissions", nil)
	fix_permissions.OnTapped = func() {
		executable, err := os.Executable()
		if err != nil {
			fmt.Printf("Error determining executable path: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Fixing permissions: %s\n", executable)
		cmd := exec.Command("pkexec", "/sbin/setcap", "cap_sys_admin,cap_dac_override,cap_setpcap=p", executable)
		_ = cmd.Run()
		os.Exit(0)
	}

	top := container.NewVBox(form, intelFastCheck, dontGrabCheck, fix_permissions, start)
	w.SetContent(container.NewBorder(top, nil, nil, nil, logs.scroll))
	w.Resize(fyne.NewSize(640, 480))

	// Closing the window exits the process, killing the server goroutine.
	w.SetOnClosed(func() { os.Exit(0) })

	w.ShowAndRun()
}
