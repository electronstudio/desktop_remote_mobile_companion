package main

import (
	"github.com/alexflint/go-arg"
	"github.com/electronstudio/desktop_remote_mobile_companion/server"
)

func main() {
	var cli server.CLI
	arg.MustParse(&cli)
	server.Run(cli)
}
