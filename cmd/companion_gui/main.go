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

func main() {
	// Default server configuration, taken from the go-arg `default:` struct
	// tags on server.CLI so the GUI starts with the same configuration as
	// `companion` with no arguments. The GUI ignores command-line flags; the
	// widgets below edit this struct before the Start button passes it to
	// server.Run.
	cli := server.CLIDefaults()

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

	form := widget.NewForm(
		widget.NewFormItem("Port", portEntry),
		widget.NewFormItem("Video source", videoSourceSelect),
		widget.NewFormItem("Video encoder", videoEncoderSelect),
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
		cmd.Run()
		os.Exit(0)
	}

	top := container.NewVBox(form, start)
	w.SetContent(container.NewBorder(top, fix_permissions, nil, nil, logs.scroll))
	w.Resize(fyne.NewSize(640, 480))

	// Closing the window exits the process, killing the server goroutine.
	w.SetOnClosed(func() { os.Exit(0) })

	w.ShowAndRun()
}
