// Command enginex-mod-helper is the loopback-only helper of the official EngineX
// Mods: one static binary per architecture (linux/arm GOARM=7, linux/arm64), no
// interpreter, no shell evaluation, no package installation.
//
//	enginex-mod-helper <mod-id> serve|health|health-once|stop|restore [mod-dir]
//
// start.sh runs it from the Mod directory. Build (from the repository root):
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags=-s\ -w -o enginex-mod-helper-armv7 ./docs/mods/src/helper
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags=-s\ -w -o enginex-mod-helper-arm64 ./docs/mods/src/helper
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/textproto"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

// Ports are fixed: each Panel.qml talks to its own.
var ports = map[string]int{"dmx-studio": 17834, "jog-logo-studio": 17835,
	"sampler-pad-studio": 17836, "minimal-club-mode": 17837,
	"midi-controller-mapper": 17838}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, format+"\n", a...)
		return 1
	}
	if len(args) < 1 {
		return fail("usage: enginex-mod-helper <mod-id> serve|health|health-once|stop|restore [mod-dir]")
	}
	mod := args[0]
	port, ok := ports[mod]
	if !ok || features[mod] == nil {
		return fail("unknown Mod")
	}
	command := "serve"
	if len(args) > 1 {
		command = args[1]
	}
	modDir, _ := os.Getwd()
	if len(args) > 2 {
		modDir = args[2]
	}
	base := os.Getenv("ENGINEX_MOD_STATE")
	if base == "" {
		base = "/data/enginex/mods-state"
	}
	base, _ = filepath.Abs(base)
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	env := modkit.Env{State: filepath.Join(base, mod), ModDir: modDir}
	switch command {
	case "health", "health-once":
		attempts := 1
		if command == "health" {
			attempts = 10
		}
		for i := 0; i < attempts; i++ {
			if probe(port) {
				return 0
			}
			time.Sleep(100 * time.Millisecond)
		}
		return fail("Mod helper did not become ready")
	case "stop":
		if err := stop(env.State, mod); err != nil {
			return fail("%v", err)
		}
		return 0
	case "restore":
		if mod != "minimal-club-mode" {
			return fail("usage: start.sh [serve]")
		}
		f, err := features[mod](env)
		if err != nil {
			return fail("%v", err)
		}
		defer f.Close()
		if _, err := f.Action(map[string]any{"action": "disable"}); err != nil {
			return fail("%v", err)
		}
		return 0
	case "serve":
		if err := serve(mod, port, env); err != nil {
			return fail("%v", err)
		}
		return 0
	}
	return fail("usage: start.sh [serve]")
}

// probe is the readiness check: GET /status answers JSON within 200 ms.
func probe(port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := fmt.Fprintf(conn, "GET /status HTTP/1.0\r\nHost: %s\r\n\r\n", addr); err != nil {
		return false
	}
	r := bufio.NewReader(conn)
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil || !strings.HasPrefix(line, "HTTP/") || len(strings.Fields(line)) < 2 || strings.Fields(line)[1] != "200" {
		return false
	}
	if _, err := tp.ReadMIMEHeader(); err != nil {
		return false
	}
	var v any
	return json.NewDecoder(r).Decode(&v) == nil
}

// stop signals the recorded helper, only if that PID really is this Mod's helper.
func stop(state, mod string) error {
	pidPath := filepath.Join(state, "helper.pid")
	data, err := os.ReadFile(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return errors.New("invalid helper PID")
	}
	proc := fmt.Sprintf("/proc/%d", pid)
	if _, err := os.Stat(proc); err == nil {
		cmdline, _ := os.ReadFile(proc + "/cmdline")
		if !ownHelper(bytes.Split(cmdline, []byte{0}), mod) {
			return errors.New("PID does not belong to this Mod; refusing to signal it")
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
		stopped := false
		for i := 0; i < 25; i++ {
			stat, err := os.ReadFile(proc + "/stat")
			if err != nil {
				stopped = true
				break
			}
			if _, rest, ok := strings.Cut(string(stat), ") "); ok && strings.HasPrefix(rest, "Z") {
				stopped = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !stopped {
			return errors.New("helper has not stopped; retry after pending I/O completes")
		}
	}
	if err := os.Remove(pidPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func ownHelper(argv [][]byte, mod string) bool {
	helper, hasMod, hasServe := false, false, false
	for _, a := range argv {
		s := string(a)
		if strings.HasPrefix(filepath.Base(s), "enginex-mod-helper") {
			helper = true
		}
		hasMod = hasMod || s == mod
		hasServe = hasServe || s == "serve"
	}
	return helper && hasMod && hasServe
}

func serve(mod string, port int, env modkit.Env) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	feature, err := features[mod](env)
	if err != nil {
		ln.Close()
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sig; ln.Close() }()
	h := handler{port: port, feature: feature}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break
			}
			continue
		}
		// One request at a time, as before: features need no request concurrency.
		h.handle(conn)
	}
	feature.Close()
	return nil
}

type handler struct {
	port    int
	feature modkit.Feature
}

var statusText = map[int]string{200: "OK", 400: "Bad Request", 403: "Forbidden", 404: "Not Found", 500: "Internal Server Error", 501: "Not Implemented"}

func reply(w io.Writer, code int, value any) {
	blob, err := json.Marshal(value)
	if err != nil {
		code, blob = 500, []byte(`{"error":"unencodable status"}`)
	}
	fmt.Fprintf(w, "HTTP/1.0 %d %s\r\nContent-Type: application/json\r\nCache-Control: no-store\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", code, statusText[code], len(blob))
	w.Write(blob)
}

func errorBody(err error) map[string]string { return map[string]string{"error": err.Error()} }

// allowed rejects browser cross-site writes and DNS rebinding; QML sends no Origin.
func (h handler) allowed(hdr textproto.MIMEHeader) bool {
	host := hdr.Get("Host")
	p := strconv.Itoa(h.port)
	if host != "127.0.0.1:"+p && host != "localhost:"+p {
		return false
	}
	origin := hdr.Get("Origin")
	return origin == "" || origin == "null" || origin == "http://127.0.0.1:"+p || origin == "http://localhost:"+p
}

func (h handler) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	r := bufio.NewReader(conn)
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return
	}
	parts := strings.Fields(line)
	if len(parts) != 3 {
		reply(conn, 400, map[string]string{"error": "bad request line"})
		return
	}
	method, target := parts[0], parts[1]
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		reply(conn, 400, map[string]string{"error": "bad headers"})
		return
	}
	if !h.allowed(hdr) {
		reply(conn, 403, map[string]string{"error": "local QML only"})
		return
	}
	switch method {
	case "GET":
		path, _, _ := strings.Cut(target, "?")
		path, _, _ = strings.Cut(path, "#")
		if path != "/status" {
			reply(conn, 404, map[string]string{"error": "unknown endpoint"})
			return
		}
		v, err := h.feature.Status()
		if err != nil {
			reply(conn, 500, errorBody(err))
			return
		}
		reply(conn, 200, v)
	case "POST":
		if target != "/action" {
			reply(conn, 404, map[string]string{"error": "unknown endpoint"})
			return
		}
		size, err := strconv.Atoi(strings.TrimSpace(hdr.Get("Content-Length")))
		ctype, _, _ := mime.ParseMediaType(hdr.Get("Content-Type"))
		if hdr.Get("Content-Type") == "" {
			ctype = "text/plain"
		}
		if err != nil || size <= 0 || size > 65536 || ctype != "application/json" {
			reply(conn, 400, map[string]string{"error": "expected a JSON body of 1..65536 bytes"})
			return
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			reply(conn, 400, errorBody(err))
			return
		}
		req, err := modkit.Decode(body)
		if err != nil {
			reply(conn, 400, errorBody(err))
			return
		}
		var v any
		if feature, ok := h.feature.(interface {
			ActionRaw(map[string]any, []byte) (any, error)
		}); ok {
			v, err = feature.ActionRaw(req, body)
		} else {
			v, err = h.feature.Action(req)
		}
		if err != nil {
			code := 500
			if modkit.IsBad(err) {
				code = 400
			}
			reply(conn, code, errorBody(err))
			return
		}
		reply(conn, 200, v)
	default:
		reply(conn, 501, map[string]string{"error": "unsupported method"})
	}
}
