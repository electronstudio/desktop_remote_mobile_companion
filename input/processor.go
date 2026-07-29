package input

// EventProcessor is a virtual input device that consumes browser events and
// can be reset on client disconnect. Both trackpad.Device and tablet.Device
// satisfy it.
//
// The signaling layer routes incoming data-channel events to a registered
// EventProcessor by Event.Device name (see the signaling package) instead of
// switching on concrete device types, so adding a device does not require
// editing the signaling code.
type EventProcessor interface {
	// ProcessEvent applies a browser touch/pointer/button event to the device.
	ProcessEvent(Event) error
	// Reset releases all active contact/tool state, as if the client lifted
	// every finger / withdrew the pen. Called on client disconnect so a
	// reconnecting client does not inherit stuck state.
	Reset()
}

// Activator is an optional capability of an EventProcessor: handling the
// client "activate" control message, which reports whether the tablet panel
// is the active panel. Only the tablet implements it.
//
// A device that does NOT implement Activator leaves "activate" events to its
// ProcessEvent (see Session.handleDataMessage), matching the original
// routing where only the tablet intercepted "activate" and every other device
// passed it straight through to ProcessEvent.
type Activator interface {
	SetActive(active bool)
}
