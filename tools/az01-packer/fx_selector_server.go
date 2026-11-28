//go:build fxselector_sandbox

// Sandbox-only HTTP process using the real production UI handlers.
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/webui"
)

func main() {
	s := webui.New(webui.Config{UIDir: os.Args[1], StateDir: os.Args[2], AccessToken: "sandbox-fixture", FXSelectorReadyFile: filepath.Join(os.Args[1], "../../../run/enginex-fx-selector.ready"),
		ColorFXReadyFile: filepath.Join(os.Args[1], "../../../run/enginex-colorfx.ready")}, nil)
	defer s.Close()
	var h http.Handler = s
	if delayMs, _ := strconv.Atoi(os.Getenv("FX_LED_POST_DELAY_MS")); delayMs > 0 {
		delay := time.Duration(delayMs) * time.Millisecond
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Query().Has("ledBrightness") {
				time.Sleep(delay)
			}
			s.ServeHTTP(w, r)
		})
	}
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", h))
}
