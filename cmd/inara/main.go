package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/alexflint/go-arg"
	"github.com/electronstudio/desktop_remote_mobile_companion/server"
)

func main() {
	var cli server.CLI

	if err := server.LoadConfig(&cli); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	p, err := arg.NewParser(arg.Config{IgnoreDefault: true}, &cli)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	p.MustParse(os.Args[1:])
	arg.MustParse(&cli)

	srv, err := server.New(cli)
	if err != nil {
		log.Fatal(err)
	}

	// The first Ctrl-C (or SIGTERM) triggers a graceful shutdown so each
	// session's FFmpeg capture pipeline is closed cleanly: tearing the
	// process down mid-capture can crash the Wayland compositor. A second
	// signal force-quits immediately, skipping the graceful path.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, server.InterruptSignals()...)
	defer signal.Stop(signals)
	go func() {
		<-signals
		go func() {
			<-signals
			os.Exit(1)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	if err := srv.Run(); err != nil {
		log.Fatal(err)
	}
}
