// Package stemsd is enginex-stemsd (alias sc5000-stemsd): it runs the Engine 5.0.4 Stems worker for remote decks.
// It listens on loopback by default; sc5000-relay is the public entry point
// and forwards authenticated /v1/engine sessions here.
//
// As a home renderer (tools/stems/run-home-worker.sh) it listens on the LAN
// with -token-file "" and answers discovery probes on UDP 17891, so decks in
// the same network find it without any setup.
package stemsd

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

func defaultBackend() string {
	return cmp.Or(os.Getenv("ENGINEX_STEMS_BACKEND"), "htdemucs")
}

// Main runs the daemon; both cmd/enginex-stemsd and the sc5000-stemsd alias call it.
func Main() {
	listen := flag.String("listen", "127.0.0.1:6596", "listen address")
	token := flag.String("token-file", defaultToken(), `internal credential file; "" serves LAN callers without one`)
	worker := flag.String("worker", "/opt/sc5000-stems/worker.py", "protocol worker")
	python := flag.String("python", "/usr/bin/python3", "Python interpreter for the worker")
	backend := flag.String("backend", defaultBackend(), "separator: htdemucs, demucs, auto or hpss")
	sessions := flag.Int("sessions", 1, "concurrent Stems sessions")
	announce := flag.Bool("lan-announce", true, "answer LAN discovery probes on UDP 17891 (never when listening on loopback)")
	flag.Parse()
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	*listen, *token = resolveListenAndToken(*listen, *token, set, os.Stderr)
	secret := ""
	if *token != "" {
		s, e := stems.Secret(*token)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		secret = s
	}
	name, args := resolveWorker(*python, *worker, *backend)
	b := &stems.Backend{Secret: secret, Worker: args, Slot: make(chan struct{}, max(1, *sessions)), Log: os.Stderr, Name: name}
	server := &http.Server{Addr: *listen, Handler: b, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	host, port, _ := net.SplitHostPort(*listen)
	if ip := net.ParseIP(host); *announce && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		p, _ := strconv.Atoi(port)
		device, _ := os.Hostname()
		beacon := stems.Beacon{Port: p, Backend: name, Hostname: device}
		if pc, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", stems.DiscoveryPort)); err != nil {
			fmt.Fprintln(os.Stderr, "enginex-stemsd: LAN discovery off:", err)
		} else {
			go stems.Announce(ctx, pc, beacon)
			fmt.Fprintf(os.Stderr, "answering LAN discovery on udp/%d\n", stems.DiscoveryPort)
		}
		// shortcut: joins the group on the default interface only; per-interface joins need x/net/ipv4 if a multi-homed PC misses it.
		if mc, err := net.ListenMulticastUDP("udp4", nil, stems.MDNSGroup); err != nil {
			fmt.Fprintln(os.Stderr, "mDNS off (UDP 17891 discovery still works):", err)
		} else {
			go stems.Announce(ctx, mc, beacon)
			fmt.Fprintln(os.Stderr, "answering mDNS for _enginex-stems._tcp.local.")
		}
	}
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(c); err != nil {
			server.Close()
		}
		close(shutdownDone)
	}()
	fmt.Fprintln(os.Stderr, "enginex-stemsd listening on", *listen, "backend", name)
	if e := server.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if ctx.Err() != nil {
		<-shutdownDone
	}
}

// resolveWorker asks worker.py which separator backend resolves to, once:
// importing torch per session only to pick one would cost seconds.
func resolveWorker(python, worker, backend string) (string, []string) {
	return resolveWorkerWithLog(python, worker, backend, os.Stderr)
}

func resolveWorkerWithLog(python, worker, backend string, log io.Writer) (string, []string) {
	probe, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	got, e := exec.CommandContext(probe, python, worker, "--backend", backend, "--print-backend").Output()
	name := strings.TrimSpace(string(got))
	if e == nil && (name == "hpss" || name == "htdemucs") {
		if backend != "hpss" && name == "hpss" {
			fmt.Fprintln(log, "enginex-stemsd: neural backend (htdemucs) Python dependencies unavailable; falling back to hpss")
		}
		return name, []string{python, "-u", worker, "--backend", name}
	}
	if backend != "hpss" {
		if gotFallback, e2 := exec.CommandContext(probe, python, worker, "--backend", "hpss", "--print-backend").Output(); e2 == nil && strings.TrimSpace(string(gotFallback)) == "hpss" {
			fmt.Fprintln(log, "enginex-stemsd: neural backend (htdemucs) Python dependencies unavailable; falling back to hpss")
			return "hpss", []string{python, "-u", worker, "--backend", "hpss"}
		}
	}
	// An older worker.py has no --backend: it only does hpss, so run it as before.
	fmt.Fprintf(log, "warning: worker backend %q: %v; assuming hpss\n", backend, cmp.Or(e, fmt.Errorf("answered %q", name)))
	return "hpss", []string{python, "-u", worker}
}

func resolveListenAndToken(listen, token string, set map[string]bool, errOut io.Writer) (string, string) {
	if !set["listen"] {
		if token == "" {
			listen = "0.0.0.0:18765"
			fmt.Fprintln(errOut, "enginex-stemsd: empty -token-file without -listen: zero-config Home LAN mode on :18765 (no auth, UDP discovery beacon on)")
		} else if !set["token-file"] {
			if _, e := os.Stat(token); e != nil {
				// Zero-config home PC: serve the LAN without a credential, with discovery.
				listen, token = "0.0.0.0:18765", ""
				fmt.Fprintln(errOut, "enginex-stemsd: no token file and no flags: zero-config Home LAN mode on :18765 (no auth, UDP discovery beacon on)")
			}
		}
	}
	return listen, token
}

// defaultToken prefers the EngineX token paths and falls back to the legacy one.
func defaultToken() string {
	for _, p := range []string{"/etc/enginex/stems.token", "/data/enginex/stems.token"} {
		if _, e := os.Stat(p); e == nil {
			return p
		}
	}
	return stems.DefaultTokenFile
}
