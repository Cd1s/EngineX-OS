// Package stemsd is enginex-stemsd (alias sc5000-stemsd): it runs the Engine 5.0.4 Stems worker for remote decks.
// It listens on loopback by default; sc5000-relay is the public entry point
// and forwards authenticated /v1/engine sessions here.
//
// As a home renderer (tools/stems/run-home-worker.sh, or standalone with -native / -backend tiered)
// it listens on the LAN with -token-file "" and answers discovery probes on UDP 17891,
// so decks in the same network find it without any setup.
package stemsd

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

func defaultBackend() string {
	return cmp.Or(os.Getenv("ENGINEX_STEMS_BACKEND"), "htdemucs")
}

func defaultModelsDir() string {
	if d := os.Getenv("ENGINEX_STEMS_MODELS_DIR"); d != "" {
		return d
	}
	if base, err := os.UserCacheDir(); err == nil && base != "" {
		return filepath.Join(base, "enginex-stems", "models")
	}
	return "/var/tmp/enginex-stems/models"
}

func defaultCacheDir() string {
	if d := os.Getenv("ENGINEX_STEMS_CACHE_DIR"); d != "" {
		return d
	}
	if base, err := os.UserCacheDir(); err == nil && base != "" {
		return filepath.Join(base, "enginex-stems", "hq")
	}
	return "/var/tmp/enginex-stems/hq"
}

func isNativeBackendName(b string) bool {
	switch b {
	case "tiered", "spleeter+htdemucs", "spleeter", "native-htdemucs", "native-hpss":
		return true
	}
	return false
}

// Main runs the daemon; both cmd/enginex-stemsd and the sc5000-stemsd alias call it.
func Main() {
	listen := flag.String("listen", "127.0.0.1:6596", "listen address")
	token := flag.String("token-file", defaultToken(), `internal credential file; "" serves LAN callers without one`)
	worker := flag.String("worker", "/opt/sc5000-stems/worker.py", "protocol worker (Python path when not running in-process)")
	python := flag.String("python", "/usr/bin/python3", "Python interpreter for worker.py")
	backend := flag.String("backend", defaultBackend(), "separator: tiered, spleeter, native-htdemucs, native-hpss, htdemucs, demucs, auto or hpss")
	native := flag.Bool("native", false, "run separator in-process without Python")
	modelsDir := flag.String("models-dir", defaultModelsDir(), "directory for MIT Spleeter / HTDemucs weights")
	cacheDir := flag.String("cache-dir", defaultCacheDir(), "directory for content-addressed HQ Stems cache (\"\" disables)")
	cacheMaxMB := flag.Int("cache-max-mb", 2048, "maximum HQ Stems cache size in MiB")
	downloadModels := flag.Bool("download-models", false, "download and SHA-256 verify MIT Spleeter + HTDemucs weights into -models-dir, then exit")
	autoDownload := flag.Bool("auto-download-models", false, "download missing MIT model weights automatically when starting a native neural backend")
	sessions := flag.Int("sessions", 1, "concurrent Stems sessions")
	announce := flag.Bool("lan-announce", true, "answer LAN discovery probes on UDP 17891 (never when listening on loopback)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *downloadModels {
		if err := ensureModels(ctx, *modelsDir, true, true, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "enginex-stemsd:", err)
			os.Exit(1)
		}
		return
	}

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["listen"] && !set["token-file"] {
		if _, e := os.Stat(*token); e != nil {
			// Zero-config home PC: serve the LAN without a credential, with discovery.
			*listen, *token = "0.0.0.0:18765", ""
			fmt.Fprintln(os.Stderr, "enginex-stemsd: no token file and no flags: zero-config Home LAN mode on :18765 (no auth, UDP discovery beacon on)")
		}
	}
	secret := ""
	if *token != "" {
		s, e := stems.Secret(*token)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		secret = s
	}

	useNative := *native || isNativeBackendName(*backend)
	if !useNative && !set["worker"] {
		if _, err := os.Stat(*worker); err != nil {
			// Standalone binary with no worker.py installed: run in-process without Python.
			useNative = true
		}
	}

	var (
		name      string
		args      []string
		renderFn  func(context.Context, io.Reader, io.Writer) error
		cleanupFn func()
	)
	if useNative {
		var pyHQ []string
		if !*native {
			if _, err := os.Stat(*worker); err == nil {
				if pyName, pyArgs := resolveWorkerWithLog(*python, *worker, "htdemucs", io.Discard); pyName == "htdemucs" {
					pyHQ = pyArgs
				}
			}
		}
		name, renderFn, cleanupFn = resolveNativeBackend(ctx, *backend, *modelsDir, *cacheDir, *cacheMaxMB, *autoDownload, pyHQ, os.Stderr)
		if cleanupFn != nil {
			defer cleanupFn()
		}
	} else {
		name, args = resolveWorker(*python, *worker, *backend)
	}

	b := &stems.Backend{
		Secret: secret,
		Worker: args,
		Render: renderFn,
		Slot:   make(chan struct{}, max(1, *sessions)),
		Log:    os.Stderr,
		Name:   name,
	}
	server := &http.Server{Addr: *listen, Handler: b, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
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

func ensureModels(ctx context.Context, modelsDir string, wantSpleeter, wantHTDemucs bool, log io.Writer) error {
	if wantSpleeter {
		dst := filepath.Join(modelsDir, "4stems.tar.gz")
		if _, err := os.Stat(dst); err != nil {
			fmt.Fprintf(log, "enginex-stemsd: downloading MIT Spleeter 4stems weights to %s ...\n", dst)
		}
		if err := stems.DownloadVerifiedAsset(ctx, stems.Spleeter4StemsURL, dst, stems.Spleeter4StemsSHA256); err != nil {
			return err
		}
		fmt.Fprintf(log, "enginex-stemsd: Spleeter 4stems verified (%s)\n", dst)
	}
	if wantHTDemucs {
		dst := filepath.Join(modelsDir, "htdemucs.safetensors")
		if _, err := os.Stat(dst); err != nil {
			fmt.Fprintf(log, "enginex-stemsd: downloading MIT HTDemucs v4 weights to %s ...\n", dst)
		}
		if err := stems.DownloadVerifiedAsset(ctx, stems.HTDemucsSafetensorsURL, dst, stems.HTDemucsSafetensorsSHA256); err != nil {
			return err
		}
		fmt.Fprintf(log, "enginex-stemsd: HTDemucs v4 verified (%s)\n", dst)
	}
	return nil
}

func loadOptionalSpleeter(ctx context.Context, modelsDir string, autoDownload bool, log io.Writer) (*stems.SpleeterModel, error) {
	if modelsDir == "" {
		return nil, errors.New("no models directory")
	}
	if m, err := stems.LoadSpleeterPath(modelsDir); err == nil {
		return m, nil
	}
	if !autoDownload {
		return nil, fmt.Errorf("Spleeter weights not found in %s (run with -download-models or -auto-download-models)", modelsDir)
	}
	if err := ensureModels(ctx, modelsDir, true, false, log); err != nil {
		return nil, err
	}
	return stems.LoadSpleeterPath(modelsDir)
}

func loadOptionalHTDemucs(ctx context.Context, modelsDir string, autoDownload bool, log io.Writer) (*stems.HTDemucsModel, error) {
	if modelsDir == "" {
		return nil, errors.New("no models directory")
	}
	stPath := filepath.Join(modelsDir, "htdemucs.safetensors")
	if m, err := stems.LoadHTDemucsSafetensors(stPath); err == nil {
		return m, nil
	}
	if !autoDownload {
		return nil, fmt.Errorf("HTDemucs weights not found at %s (run with -download-models or -auto-download-models)", stPath)
	}
	if err := ensureModels(ctx, modelsDir, false, true, log); err != nil {
		return nil, err
	}
	return stems.LoadHTDemucsSafetensors(stPath)
}

func hpssWholeTrack(ctx context.Context, x [][]float32, rate float64) ([][][]float32, error) {
	sep, err := stems.NewHPSSSeparator(len(x), rate)
	if err != nil {
		return nil, err
	}
	y1, err := sep.Feed(ctx, x)
	if err != nil {
		return nil, err
	}
	y2, err := sep.Drain(ctx)
	if err != nil {
		return nil, err
	}
	out := make([][][]float32, 4)
	for s := 0; s < 4; s++ {
		out[s] = make([][]float32, len(x))
		for c := range x {
			out[s][c] = append(y1[s][c], y2[s][c]...)
		}
	}
	return out, nil
}

func resolveNativeBackend(ctx context.Context, backend, modelsDir, cacheDir string, cacheMaxMB int, autoDownload bool, pyHQWorker []string, log io.Writer) (string, func(context.Context, io.Reader, io.Writer) error, func()) {
	nativeHPSS := func() (string, func(context.Context, io.Reader, io.Writer) error, func()) {
		fn := func(ctx context.Context, in io.Reader, out io.Writer) error {
			return stems.ServeNative(ctx, in, out, func(ch int, rate float64) (stems.StreamSeparator, error) {
				return stems.NewHPSSSeparator(ch, rate)
			})
		}
		return "hpss", fn, nil
	}

	switch backend {
	case "hpss", "native-hpss":
		return nativeHPSS()

	case "spleeter":
		sp, err := loadOptionalSpleeter(ctx, modelsDir, autoDownload, log)
		if err != nil {
			fmt.Fprintf(log, "enginex-stemsd: %v; falling back to in-process hpss\n", err)
			return nativeHPSS()
		}
		te := stems.NewTieredEngine(sp.Separate, nil, nil, log)
		return "spleeter", te.ServeSession, te.Close

	case "htdemucs", "demucs", "native-htdemucs":
		htd, err := loadOptionalHTDemucs(ctx, modelsDir, autoDownload, log)
		if err != nil {
			// Try Spleeter before falling all the way back to HPSS when in standalone mode.
			if sp, errSp := loadOptionalSpleeter(ctx, modelsDir, false, log); errSp == nil {
				fmt.Fprintf(log, "enginex-stemsd: %v; falling back to in-process spleeter\n", err)
				te := stems.NewTieredEngine(sp.Separate, nil, nil, log)
				return "spleeter", te.ServeSession, te.Close
			}
			fmt.Fprintf(log, "enginex-stemsd: %v; falling back to in-process hpss\n", err)
			return nativeHPSS()
		}
		cache, err := stems.NewHQCache(cacheDir, cacheMaxMB)
		if err != nil {
			fmt.Fprintf(log, "enginex-stemsd: HQ cache disabled (%v)\n", err)
		}
		// Direct HQ pass: checks cache first, else runs HTDemucs and stores result in cache!
		fn := func(ctx context.Context, x [][]float32, rate float64) ([][][]float32, error) {
			y, err := htd.Separate(ctx, x, rate)
			if err == nil && cache != nil {
				_ = cache.Put(stems.TrackAudioDigest(x, rate), y, rate)
			}
			return y, err
		}
		te := stems.NewTieredEngine(fn, nil, cache, log)
		return "htdemucs", te.ServeSession, te.Close

	default: // "tiered", "spleeter+htdemucs", "auto"
		cache, err := stems.NewHQCache(cacheDir, cacheMaxMB)
		if err != nil {
			fmt.Fprintf(log, "enginex-stemsd: HQ cache disabled (%v)\n", err)
		}
		var instantFn stems.WholeTrackFunc
		instantName := "hpss"
		if sp, err := loadOptionalSpleeter(ctx, modelsDir, autoDownload, log); err == nil {
			instantFn = sp.Separate
			instantName = "spleeter"
		} else {
			fmt.Fprintf(log, "enginex-stemsd: instant neural pass unavailable (%v); using in-process hpss for instant pass\n", err)
			instantFn = hpssWholeTrack
		}

		var hqFn stems.WholeTrackFunc
		hqName := ""
		if htd, err := loadOptionalHTDemucs(ctx, modelsDir, autoDownload, log); err == nil {
			hqFn = htd.Separate
			hqName = "htdemucs"
		} else if len(pyHQWorker) > 0 {
			hqFn = stems.WorkerWholeTrackFunc(pyHQWorker)
			hqName = "htdemucs"
		} else if backend == "tiered" || backend == "spleeter+htdemucs" {
			fmt.Fprintf(log, "enginex-stemsd: background htdemucs pass unavailable (%v); serving %s only\n", err, instantName)
		}

		label := instantName
		if hqFn != nil && cache != nil {
			label = instantName + "+" + hqName
		}
		te := stems.NewTieredEngine(instantFn, hqFn, cache, log)
		return label, te.ServeSession, te.Close
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

// defaultToken prefers the EngineX token paths and falls back to the legacy one.
func defaultToken() string {
	for _, p := range []string{"/etc/enginex/stems.token", "/data/enginex/stems.token"} {
		if _, e := os.Stat(p); e == nil {
			return p
		}
	}
	return stems.DefaultTokenFile
}
