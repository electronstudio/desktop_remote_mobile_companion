//go:build linux

// xdg-desktop-portal ScreenCast session handling for the "pipewire" video
// source (pure Go, via godbus). The portal flow is:
//
//	CreateSession -> SelectSources (monitor, embedded cursor) -> Start
//	  (this shows the desktop environment's "allow screen sharing" dialog)
//	-> OpenPipeWireRemote
//
// Each stage returns a Request object; the result arrives asynchronously as
// an org.freedesktop.portal.Request.Response signal on the request path. The
// request path is predictable from the client-supplied handle_token, so the
// signal match is installed before the call.
//
// The result — the PipeWire node id and a connected PipeWire remote fd —
// feeds the libpipewire capture stream (pipewire_linux.c). Closing the
// Session object after capture stops lets the portal revoke the compositor's
// stream promptly.
//
// Permissions are intentionally not persisted (no persist_mode/restore_token
// options): the user confirms sharing on every run.

package video

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	portalDestination  = "org.freedesktop.portal.Desktop"
	portalObjectPath   = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	portalScreencastIf = "org.freedesktop.portal.ScreenCast"
	portalRequestIf    = "org.freedesktop.portal.Request"
	portalSessionIf    = "org.freedesktop.portal.Session"

	// portalSourceTypeMonitor is the ScreenCast SelectSources "types" value
	// for (whole) monitors. Windows and virtual sources are not offered.
	portalSourceTypeMonitor = 1
	// portalCursorModeEmbedded asks the compositor to draw the cursor into
	// the captured frames (like ddagrab's default draw_mouse).
	portalCursorModeEmbedded = 2
)

// portalSession is an open xdg-desktop-portal ScreenCast session.
type portalSession struct {
	conn *dbus.Conn
	// sessionPath is the portal Session object; Close calls its Close
	// method.
	sessionPath dbus.ObjectPath

	// NodeID is the PipeWire node to capture; RemoteFD is the connected
	// PipeWire remote fd (owned by the caller after openScreenCastPortal
	// returns).
	NodeID   uint32
	RemoteFD int

	// CaptureSize is the monitor size reported by the portal, used only as
	// the PipeWire size-negotiation default; 0x0 when the portal did not
	// report it.
	CaptureWidth, CaptureHeight int
}

// openScreenCastPortal runs the portal consent flow and returns the session
// with the PipeWire node id and remote fd ready for pwOpen. It blocks until
// the user answers the desktop environment's share dialog.
func openScreenCastPortal() (*portalSession, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("video: pipewire: connect to D-Bus session bus: %w", err)
	}
	p := &portalSession{conn: conn}

	err = p.negotiate()
	if err != nil {
		conn.Close()
		return nil, err
	}
	return p, nil
}

// negotiate runs CreateSession -> SelectSources -> Start ->
// OpenPipeWireRemote, populating NodeID/RemoteFD/CaptureSize.
func (p *portalSession) negotiate() error {
	portal := p.conn.Object(portalDestination, portalObjectPath)

	// CreateSession.
	results, err := p.callWithResponse(portal, portalScreencastIf+".CreateSession",
		map[string]dbus.Variant{
			"session_handle_token": dbus.MakeVariant(portalToken()),
			"handle_token":         dbus.MakeVariant(portalToken()),
		})
	if err != nil {
		return portalError("create session", err)
	}

	sessionHandle, _ := results["session_handle"].Value().(string)
	if sessionHandle == "" {
		return errors.New("video: pipewire: portal returned no session handle")
	}
	p.sessionPath = dbus.ObjectPath(sessionHandle)

	// SelectSources: one monitor, cursor drawn into the frames. No
	// persist_mode/restore_token: sharing must be confirmed every run.
	if _, err := p.callWithResponse(portal, portalScreencastIf+".SelectSources",
		p.sessionPath,
		map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(portalToken()),
			"types":        dbus.MakeVariant(uint32(portalSourceTypeMonitor)),
			"multiple":     dbus.MakeVariant(false),
			"cursor_mode":  dbus.MakeVariant(uint32(portalCursorModeEmbedded)),
		}); err != nil {
		p.closeSession()
		return portalError("select sources", err)
	}

	// Start: shows the desktop environment's consent dialog.
	results, err = p.callWithResponse(portal, portalScreencastIf+".Start",
		p.sessionPath, "", // no parent window: the dialog is standalone
		map[string]dbus.Variant{"handle_token": dbus.MakeVariant(portalToken())})
	if err != nil {
		p.closeSession()
		return portalError("start session (share dialog)", err)
	}

	// The response contains a(ua{sv}): one (node_id, properties) per source
	// that the user approved.
	nodeID, width, height, err := portalParseStreams(results["streams"].Value())
	if err != nil {
		p.closeSession()
		return fmt.Errorf("video: pipewire: portal Start response: %w", err)
	}
	p.NodeID = nodeID
	p.CaptureWidth, p.CaptureHeight = width, height

	// OpenPipeWireRemote returns the connected PipeWire remote fd.
	call := portal.Call(portalScreencastIf+".OpenPipeWireRemote", 0,
		p.sessionPath, map[string]dbus.Variant{})
	if call.Err != nil {
		p.closeSession()
		return portalError("open pipewire remote", call.Err)
	}
	var fd dbus.UnixFD
	if err := call.Store(&fd); err != nil {
		p.closeSession()
		return fmt.Errorf("video: pipewire: read pipewire remote fd: %w", err)
	}
	p.RemoteFD = int(fd)
	return nil
}

// callWithResponse calls a portal method that returns a Request object and
// waits for its Response signal. The request path is derived from the
// handle_token the caller put in the options; because the token is chosen
// client-side the signal match can be installed before the call so a fast
// portal can't race ahead of the subscription.
//
// The wait has no timeout by design: for the Start request the result only
// arrives when the user answers the share dialog, which can take minutes.
func (p *portalSession) callWithResponse(obj dbus.BusObject, method string, args ...interface{}) (map[string]dbus.Variant, error) {
	// The handle token is inside the trailing options dict.
	token := ""
	if len(args) > 0 {
		if opts, ok := args[len(args)-1].(map[string]dbus.Variant); ok {
			token, _ = opts["handle_token"].Value().(string)
		}
	}
	if token == "" {
		return nil, errors.New("portal call without a handle_token")
	}
	reqPath := p.requestObjectPath(token)

	sigch := make(chan *dbus.Signal, 8)
	p.conn.Signal(sigch)
	defer p.conn.RemoveSignal(sigch)

	match := fmt.Sprintf("type='signal',sender='%s',interface='%s',member='Response',path='%s'",
		portalDestination, portalRequestIf, reqPath)
	if call := p.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, match); call.Err != nil {
		return nil, fmt.Errorf("add D-Bus signal match: %w", call.Err)
	}
	defer p.conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, match)

	call := obj.Call(method, 0, args...)
	if call.Err != nil {
		return nil, call.Err
	}
	var returned dbus.ObjectPath
	if err := call.Store(&returned); err != nil {
		return nil, fmt.Errorf("read request handle: %w", err)
	}

	for sig := range sigch {
		if sig.Name != portalRequestIf+".Response" || sig.Path != reqPath {
			continue
		}
		if len(sig.Body) != 2 {
			return nil, fmt.Errorf("unexpected Response signal payload (%d args)", len(sig.Body))
		}
		code, _ := sig.Body[0].(uint32)
		results, _ := sig.Body[1].(map[string]dbus.Variant)
		switch code {
		case 0:
			return results, nil
		case 1:
			return nil, errPortalCancelled
		case 2:
			return nil, errors.New("the session ended before the request completed")
		default:
			return nil, fmt.Errorf("portal response code %d", code)
		}
	}
	return nil, errors.New("D-Bus connection closed while waiting for a portal response")
}

// errPortalCancelled means the user dismissed or cancelled the desktop
// environment's share dialog.
var errPortalCancelled = errors.New("the screen sharing request was cancelled")

// requestObjectPath computes the Request object path for a handle token:
// /org/freedesktop/portal/desktop/request/<sender>/<token>, where <sender>
// is this connection's unique bus name with the ':' stripped and dots
// replaced by underscores.
func (p *portalSession) requestObjectPath(token string) dbus.ObjectPath {
	sender := strings.TrimPrefix(p.conn.Names()[0], ":")
	sender = strings.ReplaceAll(sender, ".", "_")
	return dbus.ObjectPath(fmt.Sprintf("%s/request/%s/%s", portalObjectPath, sender, token))
}

// portalParseStreams decodes the "streams" response value, a(ua{sv}), and
// returns the first stream's PipeWire node id and the reported monitor size
// (0x0 when unreported or unparsable — the size is only a negotiation
// default).
func portalParseStreams(v interface{}) (nodeID uint32, width, height int, err error) {
	// godbus decodes a(ua{sv}) either as []interface{} or, when it can infer
	// the element shape, as [][]interface{}; accept both.
	var streams []interface{}
	switch s := v.(type) {
	case []interface{}:
		streams = s
	case [][]interface{}:
		streams = make([]interface{}, len(s))
		for i := range s {
			streams[i] = s[i]
		}
	}
	if len(streams) == 0 {
		return 0, 0, 0, fmt.Errorf("no streams in response (got %T)", v)
	}
	stream, ok := streams[0].([]interface{})
	if !ok || len(stream) != 2 {
		return 0, 0, 0, fmt.Errorf("unexpected stream entry shape: %T", streams[0])
	}
	nodeID, ok = stream[0].(uint32)
	if !ok {
		return 0, 0, 0, fmt.Errorf("unexpected node id type: %T", stream[0])
	}
	if props, ok := stream[1].(map[string]dbus.Variant); ok {
		if size, ok := props["size"].Value().([]interface{}); ok && len(size) == 2 {
			w, ok1 := int32ish(size[0])
			h, ok2 := int32ish(size[1])
			if ok1 && ok2 {
				width, height = int(w), int(h)
			}
		}
	}
	return nodeID, width, height, nil
}

func int32ish(v interface{}) (int32, bool) {
	switch n := v.(type) {
	case int32:
		return n, true
	case int:
		return int32(n), true
	case int64:
		return int32(n), true
	case uint32:
		return int32(n), true
	}
	return 0, false
}

// portalToken returns a random unique token for Request/Session handle
// names. It is hex-only, which satisfies all portal backends' token rules.
func portalToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "inara"
	}
	return "inara_" + hex.EncodeToString(b[:])
}

// portalError wraps a portal flow error with user-actionable context,
// distinguishing "the portal is not there" from "the user said no".
func portalError(stage string, err error) error {
	switch {
	case errors.Is(err, errPortalCancelled):
		return fmt.Errorf("video: pipewire: %s: %w", stage, err)
	case strings.Contains(err.Error(), "org.freedesktop.DBus.Error.ServiceUnknown"),
		strings.Contains(err.Error(), "org.freedesktop.DBus.Error.NameHasNoOwner"):
		return fmt.Errorf("video: pipewire: %s: xdg-desktop-portal is not available (%v); "+
			"install xdg-desktop-portal plus a backend for your desktop (e.g. xdg-desktop-portal-gnome, -kde or -wlr)", stage, err)
	default:
		return fmt.Errorf("video: pipewire: %s: %w", stage, err)
	}
}

// closeSession best-effort closes the portal Session object so the portal
// can tear down the compositor's capture promptly.
func (p *portalSession) closeSession() {
	if p.conn == nil || p.sessionPath == "" {
		return
	}
	p.conn.Object(portalDestination, p.sessionPath).Call(portalSessionIf+".Close", 0)
}

// Close asks the portal to close the session and releases the D-Bus
// connection. The PipeWire capture stream (pwCapture) must already be
// stopped; the RemoteFD belongs to the caller and is not touched here.
func (p *portalSession) Close() {
	if p == nil || p.conn == nil {
		return
	}
	p.closeSession()
	p.conn.Close()
	p.conn = nil
}
