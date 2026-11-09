//go:build fxselector_sandbox

// Sandbox-only HTTP process using the real production UI handlers.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/Cd1s/EngineX-OS/pkg/webui"
)

func main() {
	s := webui.New(webui.Config{UIDir: os.Args[1], StateDir: os.Args[2], AccessToken: "sandbox-fixture"}, nil)
	defer s.Close()
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", s))
}
