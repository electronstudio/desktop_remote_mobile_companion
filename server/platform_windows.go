package server

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
