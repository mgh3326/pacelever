package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	pl "github.com/mgh3326/pacelever"
	"github.com/mgh3326/pacelever/internal/cli"
)

var version = "dev"
var commit = "unknown"

func main() {
	if len(os.Args) == 3 && os.Args[1] == "_reap" {
		pid, e := strconv.Atoi(os.Args[2])
		if e != nil || pl.RunReaper(pid) != nil {
			os.Exit(pl.Invalid)
		}
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Stdin, version, commit))
}
