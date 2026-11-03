// engine-guardian heals Engine OS 5.0.4 libraries; --status inspects them
// without changes.
package main

import (
	"os"

	"github.com/Cd1s/sc5000-custom-os/internal/cli"
)

func main() { os.Exit(cli.Guardian(os.Args[1:], os.Stdout, os.Stderr)) }
