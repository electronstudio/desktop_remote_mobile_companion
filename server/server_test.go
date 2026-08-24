package server

import (
	"testing"

	"github.com/electronstudio/desktop_remote_mobile_companion/signaling"
)

// TestSessionRegistry covers the add/remove bookkeeping Shutdown relies on:
// sessions register normally while serving, are rejected once shutting down,
// and removing every registered session drains the WaitGroup.
func TestSessionRegistry(t *testing.T) {
	s := &Server{sessions: make(map[*signaling.Session]struct{})}

	s1 := &signaling.Session{}
	s2 := &signaling.Session{}
	if !s.addSession(s1) {
		t.Fatal("addSession s1 rejected while serving")
	}
	if !s.addSession(s2) {
		t.Fatal("addSession s2 rejected while serving")
	}
	if len(s.sessions) != 2 {
		t.Fatalf("expected 2 registered sessions, got %d", len(s.sessions))
	}

	// Once shutting down, new sessions must be rejected (and must not touch
	// the WaitGroup, which Shutdown is about to Wait on).
	s.sessionsMu.Lock()
	s.shuttingDown = true
	s.sessionsMu.Unlock()
	if s.addSession(&signaling.Session{}) {
		t.Fatal("addSession accepted while shutting down")
	}
	if len(s.sessions) != 2 {
		t.Fatalf("rejected session must not register, got %d", len(s.sessions))
	}

	s.removeSession(s1)
	if len(s.sessions) != 1 {
		t.Fatalf("expected 1 registered session, got %d", len(s.sessions))
	}
	s.removeSession(s2)
	s.sessionsWG.Wait() // returns immediately only if Add/Done are balanced
}
