// Command helper-template is the smallest EngineX Mod helper: copy this folder
// into your Mod and replace the feature. The deck has no Python, no compiler and
// no package manager. A Mod is QML, an optional busybox sh start.sh, and an
// optional static helper binary for each deck CPU:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o bin/helper-armv7 .   # gen-1 and gen-2 armhf
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/helper-arm64 .         # PRIME 4 G2 / SYSTEM ONE
//
// Contract (what the official Mods and their Panel.qml use):
//   - listen on 127.0.0.1:<your fixed port> only; answer GET /status and POST /action
//     with JSON; refuse any Host other than 127.0.0.1:<port>/localhost:<port>;
//   - `helper serve` runs the server, `helper health` exits 0 once /status answers;
//   - start.sh records the PID in $ENGINEX_MOD_STATE/<id>/helper.pid and `stop`
//     signals only that PID with SIGTERM; keep state under $ENGINEX_MOD_STATE/<id>.
//   - retry EINTR on raw syscalls/ioctls (Go's preemption signals interrupt them).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const port = 17899 // pick your own; the official Mods use 17834-17838

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "health":
		for i := 0; i < 10; i++ {
			c := http.Client{Timeout: 200 * time.Millisecond}
			if r, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/status"); err == nil {
				r.Body.Close()
				if r.StatusCode == 200 {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Fprintln(os.Stderr, "helper did not become ready")
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "usage: helper serve|health")
		os.Exit(2)
	}
}

func serve() error {
	local := map[string]bool{"127.0.0.1:" + strconv.Itoa(port): true, "localhost:" + strconv.Itoa(port): true}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if o := r.Header.Get("Origin"); !local[r.Host] || (o != "" && o != "null") {
				reply(w, 403, map[string]string{"error": "local QML only"})
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /status", guard(func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"ok": true}) // your state here
	}))
	mux.HandleFunc("POST /action", guard(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil {
			reply(w, 400, map[string]string{"error": "expected a JSON object"})
			return
		}
		reply(w, 200, map[string]any{"ok": true, "action": req["action"]}) // your actions here
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sig; srv.Close() }()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
