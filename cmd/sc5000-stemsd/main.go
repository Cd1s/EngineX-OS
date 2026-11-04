// Command sc5000-stemsd runs the Engine 5.0.4 Stems worker for remote decks.
// It listens on loopback by default; sc5000-relay is the public entry point
// and forwards authenticated /v1/engine sessions here.
//
// As a home renderer (tools/stems/run-home-worker.sh) it listens on the LAN
// with -token-file "" and answers discovery probes on UDP 17891, so decks in
// the same network find it without any setup.
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
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

func main() {
	listen := flag.String("listen", "127.0.0.1:6596", "listen address")
	token := flag.String("token-file", stems.DefaultTokenFile, `internal credential file; "" serves LAN callers without one`)
	worker := flag.String("worker", "/opt/sc5000-stems/worker.py", "protocol worker")
	python := flag.String("python", "/usr/bin/python3", "Python interpreter for the worker")
	backend := flag.String("backend", cmp.Or(os.Getenv("ENGINEX_STEMS_BACKEND"), "hpss"), "separator: hpss, demucs or auto")
	sessions := flag.Int("sessions", 1, "concurrent Stems sessions")
	announce := flag.Bool("lan-announce", true, "answer LAN discovery probes on UDP 17891 (never when listening on loopback)")
	flag.Parse()
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
			fmt.Fprintln(os.Stderr, "LAN discovery off:", err)
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
	fmt.Fprintln(os.Stderr, "sc5000-stemsd listening on", *listen, "backend", name)
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
	probe, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	got, e := exec.CommandContext(probe, python, worker, "--backend", backend, "--print-backend").Output()
	name := strings.TrimSpace(string(got))
	args := []string{python, "-u", worker, "--backend", name}
	if e != nil || (name != "hpss" && name != "htdemucs") {
		// An older worker.py has no --backend: it only does hpss, so run it as before.
		fmt.Fprintf(os.Stderr, "warning: worker backend %q: %v; assuming hpss\n", backend, cmp.Or(e, fmt.Errorf("answered %q", name)))
		return "hpss", args[:3]
	}
	return name, args
}
