package main

func hasCapSysAdmin() (bool, error) {
	return true, nil
}

func onNoSuidMount() (bool, error) {
	return false, nil
}
