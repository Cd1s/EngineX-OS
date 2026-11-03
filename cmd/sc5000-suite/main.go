// sc5000-suite combines rb-bridge, stream-sync, and engine-guardian into a
// single multi-call ARMv7 static binary so it fits inside the fixed-geometry
// 5.0.4 rootfs.ext2 partition with zero partition resizing.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/Cd1s/sc5000-custom-os/internal/cli"
)

func main() {
	prog := filepath.Base(os.Args[0])
	args := os.Args[1:]
	if prog == "sc5000-suite" && len(args) > 0 {
		prog, args = args[0], args[1:]
	}
	var err error
	switch prog {
	case "rb-bridge":
		if err = cli.RBBridge(args, os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "rb-bridge:", err)
			os.Exit(1)
		}
	case "stream-sync":
		if err = cli.StreamSync(args); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Print(err)
			os.Exit(1)
		}
	case "engine-guardian":
		os.Exit(cli.Guardian(args, os.Stdout, os.Stderr))
	default:
		fmt.Fprintf(os.Stderr, "usage: sc5000-suite {rb-bridge|stream-sync|engine-guardian} [args...]\n")
		os.Exit(2)
	}
}
