// Command stems-processor stands in for Engine 5.0.4's native Stems child
// process; see internal/stemsproc. sc5000-suite carries the same code.
package main

import (
	"os"

	"github.com/Cd1s/EngineX-OS/internal/stemsproc"
)

func main() { os.Exit(stemsproc.Main(os.Args[1:])) }
