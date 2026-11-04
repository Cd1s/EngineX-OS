// Command sc5000-deemixd downloads directly from Deezer on the SC5000; see
// internal/deemixd. sc5000-suite carries the same code.
package main

import (
	"errors"
	"flag"
	"log"
	"os"

	"github.com/Cd1s/EngineX-OS/internal/deemixd"
)

func main() {
	if err := deemixd.Main(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}
