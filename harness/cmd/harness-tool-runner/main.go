package main

import (
	"os"

	"github.com/boxvtk621/homelab-telegram-panel/internal/toolrunner"
)

func main() {
	os.Exit(toolrunner.HelperMain(os.Args[1:], os.Stdin, os.Stdout))
}
