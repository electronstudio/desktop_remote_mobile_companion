package server

import "os"

func hasCapSysAdmin() (bool, error) {
	return true, nil
}

func onNoSuidMount() (bool, error) {
	return false, nil
}

func dropSudoPrivileges() error {
	return nil
}

func reExecWithSudo(cli CLI) {}

// InterruptSignals returns the process signals that should trigger a graceful
// shutdown. Windows consoles deliver only Ctrl-C (os.Interrupt); SIGTERM does
// not exist there.
func InterruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
