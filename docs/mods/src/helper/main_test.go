package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

type stub struct{ last map[string]any }

func (s *stub) Status() (any, error) { return map[string]any{"ok": true}, nil }
func (s *stub) Action(req map[string]any) (any, error) {
	s.last = req
	switch req["action"] {
	case "bad":
		return nil, modkit.Bad("nope")
	case "boom":
		return nil, errors.New("internal")
	}
	return map[string]any{"done": true}, nil
}
func (s *stub) Close() {}

func roundTrip(t *testing.T, h handler, raw string) (int, string) {
	t.Helper()
	client, server := net.Pipe()
	go func() { h.handle(server) }()
	go func() { io.WriteString(client, raw) }()
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	client.Close()
	return resp.StatusCode, string(body)
}

func TestLoopbackContract(t *testing.T) {
	s := &stub{}
	h := handler{port: 17834, feature: s}
	post := func(host, origin, ctype, body string) string {
		r := "POST /action HTTP/1.1\r\nHost: " + host + "\r\n"
		if origin != "" {
			r += "Origin: " + origin + "\r\n"
		}
		if ctype != "" {
			r += "Content-Type: " + ctype + "\r\n"
		}
		return r + "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	}
	cases := []struct {
		raw  string
		code int
		has  string
	}{
		{"GET /status HTTP/1.1\r\nHost: 127.0.0.1:17834\r\n\r\n", 200, `"ok":true`},
		{"GET /status?x=1 HTTP/1.1\r\nHost: localhost:17834\r\n\r\n", 200, `"ok":true`},
		{"GET /status HTTP/1.1\r\nHost: evil.example:17834\r\n\r\n", 403, "local QML only"},
		{"GET /other HTTP/1.1\r\nHost: 127.0.0.1:17834\r\n\r\n", 404, "unknown endpoint"},
		{post("127.0.0.1:17834", "", "application/json", `{"action":"x"}`), 200, `"done":true`},
		{post("127.0.0.1:17834", "null", "application/json; charset=utf-8", `{"action":"x"}`), 200, `"done":true`},
		{post("127.0.0.1:17834", "http://evil.example", "application/json", `{"action":"x"}`), 403, "local QML only"},
		{post("127.0.0.1:17834", "", "text/plain", `{"action":"x"}`), 400, "JSON body"},
		{post("127.0.0.1:17834", "", "", `{"action":"x"}`), 400, "JSON body"},
		{post("127.0.0.1:17834", "", "application/json", `[1]`), 400, "expected an object"},
		{post("127.0.0.1:17834", "", "application/json", `{"action":"bad"}`), 400, "nope"},
		{post("127.0.0.1:17834", "", "application/json", `{"action":"boom"}`), 500, "internal"},
		{"POST /action HTTP/1.1\r\nHost: 127.0.0.1:17834\r\nContent-Type: application/json\r\nContent-Length: 70000\r\n\r\n", 400, "JSON body"},
	}
	for _, c := range cases {
		code, body := roundTrip(t, h, c.raw)
		if code != c.code || !strings.Contains(body, c.has) {
			t.Errorf("%q: %d %s", strings.SplitN(c.raw, "\r\n", 2)[0], code, body)
		}
	}
}

func TestProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			handler{port: port, feature: &stub{}}.handle(c)
		}
	}()
	if !probe(port) {
		t.Fatal("ready helper not detected")
	}
	ln.Close()
	if probe(port) {
		t.Fatal("closed port reported ready")
	}
}

func TestStopRefusesForeignPID(t *testing.T) {
	state := t.TempDir()
	// PID 1 is never ours; this process (the test binary) is not a Mod helper either.
	os.WriteFile(filepath.Join(state, "helper.pid"), []byte("1\n"), 0o644)
	if err := stop(state, "dmx-studio"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("pid 1: %v", err)
	}
	os.WriteFile(filepath.Join(state, "helper.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	if err := stop(state, "dmx-studio"); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("foreign pid: %v", err)
	}
	os.Remove(filepath.Join(state, "helper.pid"))
	if err := stop(state, "dmx-studio"); err != nil {
		t.Fatalf("no pid file: %v", err)
	}
	if !ownHelper([][]byte{[]byte("/data/mods/x/bin/enginex-mod-helper-armv7"), []byte("dmx-studio"), []byte("serve"), []byte("/data/mods/x")}, "dmx-studio") {
		t.Fatal("own helper not recognised")
	}
	if ownHelper([][]byte{[]byte("/usr/bin/python3"), []byte("runtime.py"), []byte("dmx-studio"), []byte("serve")}, "dmx-studio") {
		t.Fatal("python helper treated as ours")
	}
}

func TestEveryModHasAFeature(t *testing.T) {
	for mod := range ports {
		if features[mod] == nil {
			t.Errorf("%s has no feature", mod)
		}
	}
}
