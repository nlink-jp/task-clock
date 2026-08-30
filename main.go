package main

import (
	"os"

	"github.com/nlink-jp/task-clock/cmd"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cmd.Run(os.Args[1:], os.Stdout, os.Stderr, version))
}
