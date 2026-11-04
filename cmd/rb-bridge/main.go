// rb-bridge converts a mounted Rekordbox export into an Engine OS library.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Cd1s/EngineX-OS/internal/cli"
)

func main() {
	if err := cli.RBBridge(os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "rb-bridge:", err)
		os.Exit(1)
	}
}
