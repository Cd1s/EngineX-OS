// sc5000-suite combines rb-bridge, stream-sync, and engine-guardian into a
// single multi-call ARMv7 static binary so it fits inside the fixed-geometry
// 5.0.4 rootfs.ext2 partition with zero partition resizing.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/analyzer"
	"github.com/Cd1s/sc5000-custom-os/pkg/bridge"
	"github.com/Cd1s/sc5000-custom-os/pkg/dbhealer"
	"github.com/Cd1s/sc5000-custom-os/pkg/deemixclient"
	"github.com/Cd1s/sc5000-custom-os/pkg/enginedb"
	"github.com/Cd1s/sc5000-custom-os/pkg/prolink"
	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
	"github.com/Cd1s/sc5000-custom-os/pkg/resourceguard"
	"github.com/Cd1s/sc5000-custom-os/pkg/webui"
)

func main() {
	prog := filepath.Base(os.Args[0])
	args := os.Args[1:]
	if prog == "sc5000-suite" && len(args) > 0 {
		prog = args[0]
		args = args[1:]
	}
	switch prog {
	case "rb-bridge":
		if err := runRBBridge(args); err != nil {
			fmt.Fprintln(os.Stderr, "rb-bridge:", err)
			os.Exit(1)
		}
	case "stream-sync":
		if err := runStreamSync(args); err != nil {
			log.Print(err)
			os.Exit(1)
		}
	case "engine-guardian":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		os.Exit(runGuardian(ctx, args, os.Stdout, os.Stderr))
	default:
		fmt.Fprintf(os.Stderr, "usage: sc5000-suite {rb-bridge|stream-sync|engine-guardian} [args...]\n")
		os.Exit(2)
	}
}

func runRBBridge(args []string) error {
	fs := flag.NewFlagSet("rb-bridge", flag.ContinueOnError)
	mount := fs.String("mount-path", "", "mounted USB/export root")
	watch := fs.Bool("watch", false, "watch attached media (imports once per attachment)")
	roots := fs.String("watch-roots", "/media,/run/media,/mnt", "comma-separated mount parent directories")
	link := fs.Bool("prolink", false, "listen for Rekordbox / Pro DJ Link announcements and player status")
	bind := fs.String("bind", "0.0.0.0", "IPv4 address for Pro DJ Link listeners")
	smart := fs.Uint("smart-match", 0, "optional recommendation playlist relative to this Rekordbox track ID")
	poll := fs.Duration("poll-interval", 2*time.Second, "mount polling interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *smart > 0xffffffff {
		return fmt.Errorf("smart-match track ID exceeds uint32")
	}
	opts := enginedb.Options{SmartMatch: *smart != 0, CurrentTrack: uint32(*smart)}
	if !*watch && !*link && *mount == "" {
		fs.Usage()
		return fmt.Errorf("specify --mount-path, --watch or --prolink")
	}
	var ip net.IP
	if *link {
		ip = net.ParseIP(*bind)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("bind must be IPv4")
		}
	}
	var rs []string
	if *watch {
		rs = strings.Split(*roots, ",")
		if *mount != "" {
			rs = []string{*mount}
		}
		for i, root := range rs {
			rs[i] = strings.TrimSpace(root)
			if rs[i] == "" {
				return fmt.Errorf("watch roots must not be empty")
			}
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	emit := func(v any) { mu.Lock(); defer mu.Unlock(); _ = enc.Encode(v) }
	if *mount != "" && !*watch {
		r, e := bridge.Convert(ctx, *mount, opts)
		if e != nil {
			return e
		}
		emit(r)
	}
	if !*watch && !*link {
		return nil
	}
	errs := make(chan error, 2)
	workers := 0
	if *watch {
		workers++
		go func() {
			errs <- bridge.Watch(ctx, rs, opts, *poll, func(e bridge.WatchEvent) {
				if e.Err != nil {
					emit(map[string]any{"mount": e.Mount, "error": e.Err.Error()})
				} else {
					emit(e)
				}
			})
		}()
	}
	if *link {
		workers++
		go func() { errs <- prolink.NewDiscovery().Listen(ctx, ip, func(d prolink.Device) { emit(d) }) }()
	}
	var first error
	for i := 0; i < workers; i++ {
		e := <-errs
		if e != nil && !errors.Is(e, context.Canceled) && first == nil {
			first = e
		}
		cancel()
	}
	return first
}

// Downloads go to the front SD when present, otherwise to internal storage.
const (
	defaultDownloads  = "/media/sc5000-sd-cache/Music/Stream"
	fallbackDownloads = "/data/sc5000-custom/Stream"
	// Internal storage is shared with Engine; the SD card has room to spare.
	maxInternalCache = 3 << 30
)

func runStreamSync(args []string) error {
	fs := flag.NewFlagSet("stream-sync", flag.ContinueOnError)
	cfg := resourceguard.DefaultConfig()
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/root"
	}
	listen := fs.String("listen", ":8080", "touch/LAN HTTP listener")
	public := fs.String("public-url", "http://sc5000.lan:8080", "public console URL")
	dbPath := fs.String("database", analyzer.DefaultDatabase, "existing Engine 5.0.4 m.db (back up before use)")
	downloads := fs.String("downloads", defaultDownloads, "Deemix output root on the same SD volume")
	base := fs.String("deemix-url", "http://127.0.0.1:6595", "local REST bridge URL")
	queue := fs.String("deemix-queue", "/api/queue", "REST bridge queue path")
	mode := fs.String("deemix-mode", "queue", "queue REST bridge or directory (/api/download + exact ID filenames)")
	socket := fs.String("deemix-socket", "", "optional local Unix HTTP socket")
	pid := fs.Int("deemix-pid", 0, "also isolate all existing threads of this Deemix PID")
	lossy := fs.Bool("allow-320k", true, "accept verified MP3 when FLAC is unavailable")
	relay := fs.String("relay-url", webui.DefaultRelayURL, "cloud login relay; empty keeps logins on the LAN")
	token := fs.String("access-token", "", "console access code (default random per launch)")
	state := fs.String("state-dir", webui.DefaultStateDir(home), "private owner cookie state")
	spotifyToken := fs.String("spotify-token", filepath.Join(webui.DefaultStateDir(home), "spotify-token.json"), "owner OAuth token file; guests never use it")
	clientID := fs.String("spotify-client-id", "", "registered Spotify public PKCE client ID")
	redirect := fs.String("spotify-redirect-url", "", "exact registered HTTPS callback URL (or device loopback for local browser)")
	netease := fs.String("netease-url", "", "optional NeteaseCloudMusicApi-compatible service; empty talks to music.163.com")
	fs.IntVar(&cfg.CPU, "cpu", 3, "dedicated background CPU")
	fs.DurationVar(&cfg.ThrottleWork, "throttle-work", cfg.ThrottleWork, "work interval before cooperative sleep")
	fs.DurationVar(&cfg.ThrottleSleep, "throttle-sleep", cfg.ThrottleSleep, "cooperative sleep interval")
	fs.BoolVar(&cfg.Disabled, "disable-guard", false, "development only: disable OS resource controls")
	fs.BoolVar(&cfg.StrictCPU, "strict-cpu", false, "refuse work when the requested isolated CPU is unavailable")
	if err = fs.Parse(args); err != nil {
		return err
	}
	if *mode != "queue" && *mode != "directory" {
		return fmt.Errorf("invalid --deemix-mode")
	}
	if cfg.StrictCPU && cfg.Disabled {
		return fmt.Errorf("--strict-cpu conflicts with --disable-guard")
	}
	if cfg.CPU < 0 || cfg.ThrottleWork < 0 || cfg.ThrottleSleep < 0 || *pid < 0 {
		return fmt.Errorf("invalid resource guard configuration")
	}
	webui.UnblockRadios("/sys/class/rfkill")
	runtime.GOMAXPROCS(1)
	guard := resourceguard.New(cfg)
	reports, err := guard.ApplyToProcess(0)
	if err != nil {
		return err
	}
	for _, r := range reports {
		log.Printf("stream-sync guard: %s", r.String())
	}
	if *pid > 0 {
		reports, err = guard.ApplyToProcess(*pid)
		if err != nil {
			return err
		}
		for _, r := range reports {
			log.Printf("Deemix guard: %s", r.String())
		}
	}
	// The TIDAL gateway and console must run without a front SD card; only
	// importing into an Engine library needs one.
	var importer webui.Importer
	var internalCache int64
	if library, err := analyzer.OpenLibrary(*dbPath); err != nil {
		log.Printf("stream-sync: library import disabled: %v", err)
		if *downloads == defaultDownloads {
			*downloads = fallbackDownloads
			internalCache = maxInternalCache
		}
	} else {
		defer library.Close()
		importer = library
	}
	if err = os.MkdirAll(*downloads, 0755); err != nil {
		return err
	}
	dl := &deemixclient.Client{Mode: *mode, BaseURL: *base, QueuePath: *queue, DownloadDir: *downloads, AllowLossy: *lossy, MaxCache: internalCache}
	if *socket != "" {
		dl.HTTP = deemixclient.UnixHTTP(*socket)
	}
	bpm := &providers.BPMStore{Path: filepath.Join(*state, "bpm-cache.json"), ConfigPath: filepath.Join(*state, "bpm-config.json"), PreviewBPM: webui.PreviewAnalyzer(guard)}
	jobs := webui.NewJobs(webui.Pipeline{Guard: guard, Catalog: &providers.Deezer{}, Deemix: dl, Library: importer})
	defer jobs.Close()
	ui := webui.New(webui.Config{AccessToken: *token, PublicURL: *public, SpotifyClientID: *clientID, SpotifyRedirectURL: *redirect, SpotifyTokenPath: *spotifyToken, NeteaseURL: *netease, StateDir: *state, RelayURL: *relay, BPM: bpm}, jobs)
	fmt.Fprintf(os.Stderr, "Stream Sync: %s\n控制台访问码: %s\n", *public, ui.AccessToken())
	server := &http.Server{Addr: *listen, Handler: ui, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go ui.SyncRelaySettings(ctx)
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err = <-done:
		if err != http.ErrServerClosed {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := webui.ShutdownContext()
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			return err
		}
	}
	return nil
}

type guardianResult struct {
	dbhealer.Report
	Error string `json:"error,omitempty"`
}

func runGuardian(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("engine-guardian", flag.ContinueOnError)
	fs.SetOutput(errOut)
	mount := fs.String("heal-mount", "", "mounted volume containing Engine Library/Database2")
	watch := fs.Bool("watch", false, "watch mounted libraries; wait for quiet writes before healing")
	status := fs.Bool("status", false, "inspect libraries and print JSON without changes")
	root := fs.String("mount-root", "/media", "mount discovery root (watch/status)")
	interval := fs.Duration("interval", 10*time.Second, "watch polling interval")
	quiet := fs.Duration("quiet-for", 5*time.Second, "minimum quiet time before healing")
	dataOnly := fs.Bool("data-only", false, "repair table data but leave Engine's views, triggers and indexes untouched")
	sessions := fs.Int("max-sessions", dbhealer.DefaultMaxSessions, "maximum history sessions; 0 uses 50")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *interval <= 0 || *quiet < 0 || *sessions < 0 || (*watch && *status) || (*mount == "" && !*watch && !*status) {
		fmt.Fprintln(errOut, "use --heal-mount DIR, --watch, or --status; invalid option combination")
		return 2
	}
	discover := func(r string) []string {
		var mounts []string
		_ = filepath.WalkDir(r, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(r, path)
			depth := 0
			if rel != "." {
				depth = len(strings.Split(rel, string(filepath.Separator)))
			}
			if depth > 2 {
				return filepath.SkipDir
			}
			if s, e := os.Stat(filepath.Join(path, "Engine Library", "Database2", "m.db")); e == nil && !s.IsDir() {
				mounts = append(mounts, path)
				return filepath.SkipDir
			}
			return nil
		})
		return mounts
	}
	inspect := func(m string) guardianResult {
		r, err := dbhealer.InspectMount(m)
		res := guardianResult{Report: r}
		if err != nil {
			res.Error = err.Error()
		}
		return res
	}
	heal := func(m string, n int) guardianResult {
		healMount := dbhealer.HealMount
		if *dataOnly {
			healMount = dbhealer.HealMountDataOnly
		}
		r, err := healMount(m, n)
		res := guardianResult{Report: r}
		if err != nil {
			res.Error = err.Error()
			res.Healthy = false
		}
		return res
	}
	signature := func(m string) (string, error) {
		var b strings.Builder
		for _, name := range []string{"m.db", "m.db-wal", "m.db-journal", "hm.db", "hm.db-wal", "hm.db-journal"} {
			path := filepath.Join(m, "Engine Library", "Database2", name)
			s, err := os.Stat(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) && name != "m.db" {
					continue
				}
				return "", err
			}
			fmt.Fprintf(&b, "%s:%d:%d;", name, s.Size(), s.ModTime().UnixNano())
		}
		return b.String(), nil
	}
	enc := json.NewEncoder(out)
	mounts := func() []string {
		if *mount != "" {
			return []string{*mount}
		}
		return discover(*root)
	}
	if !*watch {
		results := []guardianResult{}
		code := 0
		for _, m := range mounts() {
			var r guardianResult
			if *status {
				r = inspect(m)
			} else {
				r = heal(m, *sessions)
			}
			if r.Error != "" {
				code = 1
			}
			results = append(results, r)
		}
		if err := enc.Encode(results); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		return code
	}
	type observed struct {
		signature, done string
		since           time.Time
	}
	seen := map[string]observed{}
	timer := time.NewTicker(*interval)
	defer timer.Stop()
	for {
		present := map[string]bool{}
		for _, m := range mounts() {
			present[m] = true
			sig, err := signature(m)
			if err != nil {
				delete(seen, m)
				continue
			}
			s, ok := seen[m]
			if !ok || sig != s.signature {
				s.signature = sig
				s.since = time.Now()
				seen[m] = s
				continue
			}
			if s.done == sig || time.Since(s.since) < *quiet {
				continue
			}
			r := heal(m, *sessions)
			if err = enc.Encode(r); err != nil {
				fmt.Fprintln(errOut, err)
				return 1
			}
			if r.Error == "" {
				s.done, _ = signature(m)
				s.signature = s.done
			}
			s.since = time.Now()
			seen[m] = s
		}
		for m := range seen {
			if !present[m] {
				delete(seen, m)
			}
		}
		select {
		case <-ctx.Done():
			return 0
		case <-timer.C:
		}
	}
}
