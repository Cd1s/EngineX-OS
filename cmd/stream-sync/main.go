// stream-sync is an optional on-device playlist/download/analyze console.
package main

import (
	"errors"
	"flag"
	"log"
	"os"

	"github.com/Cd1s/EngineX-OS/internal/cli"
)

func main() {
	if err := cli.StreamSync(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Print(err)
		os.Exit(1)
	}
}
