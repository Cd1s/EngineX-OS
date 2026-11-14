// Package stemsd is enginex-stemsd (alias sc5000-stemsd): it runs the Engine 5.0.4 Stems worker for remote decks.
// It listens on loopback by default; sc5000-relay is the public entry point
// and forwards authenticated /v1/engine sessions here.
//
// As a home renderer (tools/stems/run-home-worker.sh) it listens on the LAN
// with -token-file "" and answers discovery probes on UDP 17891, so decks in
// the same network find it without any setup.
package stemsd

import (
	"bytes"
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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

const defaultWorkerPath = "/opt/enginex-stems/worker.py"
const legacyWorkerPath = "/opt/sc5000-stems/worker.py"

func defaultBackend() string {
	return cmp.Or(os.Getenv("ENGINEX_STEMS_BACKEND"), "htdemucs")
}

// Main runs the daemon; both cmd/enginex-stemsd and the sc5000-stemsd alias call it.
func Main() {
	listen := flag.String("listen", "127.0.0.1:6596", "listen address")
	token := flag.String("token-file", defaultToken(), `internal credential file; "" serves LAN callers without one`)
	worker := flag.String("worker", defaultWorkerPath, "protocol worker")
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
	*worker = resolveWorkerPath(*worker, set["worker"], os.Executable, legacyWorkerPath)
	*python = resolvePythonPath(*python, set["python"], runtime.GOOS, os.Getenv("ENGINEX_STEMS_HOME"), os.UserHomeDir, exec.LookPath)
	name, args, err := resolveWorkerWithLog(*python, *worker, *backend, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
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
func resolveWorker(python, worker, backend string) (string, []string, error) {
	return resolveWorkerWithLog(python, worker, backend, os.Stderr)
}

func resolveWorkerPath(flagVal string, flagSet bool, exePath func() (string, error), legacyPath string) string {
	if flagSet {
		return flagVal
	}
	for _, path := range []string{flagVal, legacyPath} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	if exePath != nil {
		if exe, err := exePath(); err == nil && exe != "" {
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				exe = resolved
			}
			cand := filepath.Join(filepath.Dir(exe), "worker.py")
			if info, err := os.Stat(cand); err == nil && !info.IsDir() {
				return cand
			}
		}
	}
	return flagVal
}

func resolvePythonPath(flagVal string, flagSet bool, goos, stemsHomeEnv string, userHome func() (string, error), lookPath func(string) (string, error)) string {
	if flagSet {
		return flagVal
	}
	stemsHome := stemsHomeEnv
	if stemsHome == "" && userHome != nil {
		if home, err := userHome(); err == nil && home != "" {
			stemsHome = filepath.Join(home, ".enginex-stems")
		}
	}
	if stemsHome != "" {
		var cands []string
		if goos == "windows" {
			cands = []string{filepath.Join(stemsHome, "venv", "Scripts", "python.exe")}
		} else {
			cands = []string{
				filepath.Join(stemsHome, "venv", "bin", "python"),
				filepath.Join(stemsHome, "venv", "bin", "python3"),
			}
		}
		for _, cand := range cands {
			if info, err := os.Stat(cand); err == nil && !info.IsDir() {
				return cand
			}
		}
	}
	if info, err := os.Stat(flagVal); err == nil && !info.IsDir() {
		return flagVal
	}
	if lookPath != nil {
		names := []string{"python3", "python"}
		if goos == "windows" {
			names = []string{"python.exe", "python3", "python"}
		}
		for _, n := range names {
			if p, err := lookPath(n); err == nil && p != "" {
				return p
			}
		}
	}
	return flagVal
}

func probeBackend(ctx context.Context, python, worker, backend string) (string, string, error) {
	cmd := exec.CommandContext(ctx, python, worker, "--backend", backend, "--print-backend")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return strings.TrimSpace(outBuf.String()), strings.TrimSpace(errBuf.String()), err
}

func lastLine(s string) string {
	var last string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			last = t
		}
	}
	return last
}

func launcherHint(goos string) string {
	switch goos {
	case "darwin":
		return "run stems/run-home-worker.command (or ./stems/run-home-worker.sh in Terminal) to set up ~/.enginex-stems/venv with the required Python packages"
	case "windows":
		return `run stems\run-home-worker.ps1 (powershell -ExecutionPolicy Bypass -File .\stems\run-home-worker.ps1) to set up %USERPROFILE%\.enginex-stems\venv with the required Python packages`
	default:
		return "run ./stems/run-home-worker.sh to set up ~/.enginex-stems/venv with the required Python packages"
	}
}

func resolveWorkerWithLog(python, worker, backend string, log io.Writer) (string, []string, error) {
	if info, err := os.Stat(worker); err != nil || info.IsDir() {
		return "", nil, fmt.Errorf("enginex-stemsd: worker script %q not found (%v); %s", worker, cmp.Or(err, fmt.Errorf("is a directory")), launcherHint(runtime.GOOS))
	}
	probe, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name, stderr, e := probeBackend(probe, python, worker, backend)
	if e == nil && (name == "hpss" || name == "htdemucs") {
		if backend != "hpss" && name == "hpss" {
			fmt.Fprintln(log, "enginex-stemsd: neural backend (htdemucs) Python dependencies unavailable; falling back to hpss")
		}
		return name, []string{python, "-u", worker, "--backend", name}, nil
	}
	var stderr2 string
	if backend != "hpss" {
		if gotFallback, s2, e2 := probeBackend(probe, python, worker, "hpss"); e2 == nil && gotFallback == "hpss" {
			fmt.Fprintln(log, "enginex-stemsd: neural backend (htdemucs) Python dependencies unavailable; falling back to hpss")
			return "hpss", []string{python, "-u", worker, "--backend", "hpss"}, nil
		} else {
			stderr2 = s2
		}
	}
	// An older worker.py has no --backend: it only does hpss, so run it as before.
	if strings.Contains(stderr, "unrecognized arguments") || strings.Contains(stderr2, "unrecognized arguments") {
		fmt.Fprintf(log, "warning: worker backend %q: %v; assuming hpss\n", backend, cmp.Or(e, fmt.Errorf("answered %q", name)))
		return "hpss", []string{python, "-u", worker}, nil
	}
	detail := lastLine(cmp.Or(stderr2, stderr))
	if detail == "" {
		if e != nil {
			detail = e.Error()
		} else {
			detail = fmt.Sprintf("answered %q", name)
		}
	}
	return "", nil, fmt.Errorf("enginex-stemsd: Python %q cannot run Stems backend %q with %q (%s); %s", python, backend, worker, detail, launcherHint(runtime.GOOS))
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
