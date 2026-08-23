package server

// Optional shared-passcode ("passphrase") protection.
//
// When a passcode is configured (--passcode / INARA_PASSCODE), every client
// must authenticate once by POSTing the passcode to /auth. On success the
// server sets an HttpOnly session cookie whose value is a random token
// generated at server startup, so a server restart invalidates all cookies
// without any server-side session store. The /signal WebSocket endpoint then
// requires that cookie before the upgrade: the browser attaches cookies to
// the WebSocket handshake automatically (it is an ordinary same-origin HTTPS
// request). Because the only way to establish a WebRTC peer connection is to
// exchange SDP through /signal, gating /signal also gates the (already
// DTLS-encrypted) data channel.
//
// With no passcode configured the gate is a no-op and behaviour is unchanged.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

const (
	// authCookieName identifies the session cookie.
	authCookieName = "inara_session"
	// authCookieMaxAge makes the cookie persistent (30 days). It still dies
	// when the server restarts, because the token is regenerated per run.
	authCookieMaxAge = 30 * 24 * time.Hour
	// authFailDelay thwarts brute-force guessing over the LAN: every failed
	// /auth attempt costs the client one second.
	authFailDelay = time.Second
)

// authGate is a dependency-free authenticator for one shared passcode. The
// zero/empty-passcode gate is disabled (everything allowed, /auth still
// answers status queries so the client's logic is uniform).
type authGate struct {
	passcode []byte // empty = disabled
	token    string // random per-run session cookie value
}

// newAuthGate returns a gate enforcing the given passcode, or a disabled
// gate when passcode is empty. The passcode value is never logged.
func newAuthGate(passcode string) *authGate {
	g := &authGate{}
	if passcode == "" {
		return g
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("failed to generate session token: %v", err)
	}
	g.passcode = []byte(passcode)
	g.token = hex.EncodeToString(buf)
	return g
}

// enabled reports whether a passcode is configured.
func (g *authGate) enabled() bool { return len(g.passcode) > 0 }

// check reports whether r carries a valid session cookie (or the gate is
// disabled). It is used to gate /signal before the WebSocket upgrade.
func (g *authGate) check(r *http.Request) bool {
	if !g.enabled() {
		return true
	}
	c, err := r.Cookie(authCookieName)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(g.token)) == 1
}

// authStatus is the GET /auth response: the client uses it on page load to
// decide whether to show the passcode screen.
type authStatus struct {
	Required      bool `json:"required"`
	Authenticated bool `json:"authenticated"`
}

// ServeHTTP handles /auth:
//
//	GET  — JSON authStatus so the client knows whether to prompt.
//	POST — JSON {"passcode": "..."}; on success sets the session cookie.
func (g *authGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		_ = json.NewEncoder(w).Encode(authStatus{Required: g.enabled(), Authenticated: g.check(r)})
	case http.MethodPost:
		if !g.enabled() {
			// Nothing to authenticate against; treat as success so a client
			// mid-prompt against a reconfigured server is not stuck.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			Passcode string `json:"passcode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
			subtle.ConstantTimeCompare([]byte(req.Passcode), g.passcode) != 1 {
			log.Printf("auth: rejected passcode attempt from %s", r.RemoteAddr)
			time.Sleep(authFailDelay)
			http.Error(w, "invalid passcode", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     authCookieName,
			Value:    g.token,
			Path:     "/",
			MaxAge:   int(authCookieMaxAge.Seconds()),
			HttpOnly: true, // not readable from JS
			Secure:   true, // HTTPS only (the server speaks nothing else)
			SameSite: http.SameSiteStrictMode,
		})
		log.Printf("auth: %s authenticated", r.RemoteAddr)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
