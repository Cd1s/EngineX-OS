package cli

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
	"github.com/Cd1s/EngineX-OS/pkg/webui"
)

// Downloads go to the front SD when present, otherwise to internal storage.
const (
	defaultDownloads  = "/media/sc5000-sd-cache/Music/Stream"
	fallbackDownloads = "/data/enginex/Stream"
	// Internal storage is shared with Engine; the SD card has room to spare.
	maxInternalCache = 3 << 30
	// Engine needs room on /data for its databases and its own temp copies.
	internalMinFree = 1 << 30
	defaultDeemixd  = "/usr/local/bin/enginex-deemixd"
)

// StreamSync runs the on-device playlist/download/analyze console.
func StreamSync(args []string) error {
	fs := flag.NewFlagSet("stream-sync", flag.ContinueOnError)
	settingsCopyPrestart := fs.Bool("settings-copy-prestart", false, "prepare a confirmed settings import before Engine starts")
	cfg := resourceguard.DefaultConfig()
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/root"
	}
	listen := fs.String("listen", ":8080", "touch/LAN HTTP listener")
	public := fs.String("public-url", "http://enginex.lan:8080", "public console URL")
	dbPath := fs.String("database", analyzer.DefaultDatabase, "existing Engine 5.0.4 m.db (back up before use)")
	downloads := fs.String("downloads", defaultDownloads, "Deemix output root on the same SD volume")
	base := fs.String("deemix-url", deemixdURL, "local REST bridge URL")
	queue := fs.String("deemix-queue", "/api/queue", "REST bridge queue path")
	mode := fs.String("deemix-mode", "queue", "queue REST bridge or directory (/api/download + exact ID filenames)")
	socket := fs.String("deemix-socket", "", "optional local Unix HTTP socket")
	pid := fs.Int("deemix-pid", 0, "also isolate all existing threads of this Deemix PID")
	deemixd := fs.String("deemixd", defaultDeemixd, "start and keep this enginex-deemixd running for "+deemixdURL+"; empty leaves it to others")
	lossy := fs.Bool("allow-320k", true, "accept verified MP3 when FLAC is unavailable")
	relay := fs.String("relay-url", webui.DefaultRelayURL, "cloud login relay; empty keeps logins on the LAN")
	token := fs.String("access-token", "", "console access code (default: kept in <state-dir>/access-token)")
	state := fs.String("state-dir", webui.DefaultStateDir(home), "private owner cookie state")
	spotifyToken := fs.String("spotify-token", "", "owner OAuth token file; guests never use it (default <state-dir>/spotify-token.json)")
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
	if *settingsCopyPrestart {
		return webui.SettingsCopyBeforeEngine()
	}
	if *spotifyToken == "" {
		*spotifyToken = filepath.Join(*state, "spotify-token.json") // follows --state-dir (#31)
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
	// CPU-heavy Go workers, runtime/GC helpers and HTTP threads share the same
	// background affinity. Never set affinity for Engine/ALSA/Qt processes here.
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
		if reports, err = guard.ApplyToProcess(*pid); err != nil {
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The player's systemd reads its units before the writable /etc overlay is
	// mounted, so a unit enabled there never starts at boot. Starting the
	// downloader here makes it part of stream-sync, which the base /etc enables.
	if *deemixd != "" && *socket == "" && sameDeemixd(*base) {
		go superviseDeemixd(ctx, deemixdAddr, *deemixd, "-state-dir", *state, "-download-dir", *downloads)
	}
	dl := &deemixclient.Client{Mode: *mode, BaseURL: *base, QueuePath: *queue, DownloadDir: *downloads, AllowLossy: *lossy, MaxCache: internalCache, MinFree: internalMinFree}
	if *socket != "" {
		dl.HTTP = deemixclient.UnixHTTP(*socket)
	}
	bpm := &providers.BPMStore{Path: filepath.Join(*state, "bpm-cache.json"), ConfigPath: filepath.Join(*state, "bpm-config.json"), PreviewBPM: webui.PreviewAnalyzer(guard)}
	jobs := webui.NewJobs(webui.Pipeline{Guard: guard, Catalog: &providers.Deezer{}, Deemix: dl, Library: importer})
	defer jobs.Close()
	newToken, explicitToken := false, *token != ""
	if !explicitToken {
		if *token, newToken, err = webui.LoadAccessToken(*state); err != nil {
			return err
		}
	}
	ui := webui.New(webui.Config{AccessToken: *token, PublicURL: *public, SpotifyClientID: *clientID, SpotifyRedirectURL: *redirect, SpotifyTokenPath: *spotifyToken, NeteaseURL: *netease, StateDir: *state, RelayURL: *relay, BPM: bpm}, jobs)
	defer ui.Close()
	// This is the one intentional local credential output: the operator needs
	// it to pair the touch/phone console. No provider credentials are logged.
	// Printed once, when created; afterwards only where to find it.
	switch {
	case newToken:
		fmt.Fprintf(os.Stderr, "Stream Sync: %s\n控制台访问码: %s\n", *public, ui.AccessToken())
	case explicitToken:
		fmt.Fprintf(os.Stderr, "Stream Sync: %s\n控制台访问码: 由 --access-token 指定\n", *public)
	default:
		fmt.Fprintf(os.Stderr, "Stream Sync: %s\n控制台访问码: 见 %s\n", *public, filepath.Join(*state, "access-token"))
	}
	server := &http.Server{Addr: *listen, Handler: ui, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	go ui.SyncRelaySettings(ctx)
	go ui.RestoreSSH(ctx) // first generation: the console's SSH settings after a boot
	go webui.GuardRoutes(ctx)
	dl.RemoveStaleParts()
	dl.Trim("")
	go watchEngineTemp(ctx)
	go watchPersistentClock(ctx)
	// Tunnels and Tailscale forward to a loopback console listener of their own,
	// which has no route to the deck-local streaming gateway.
	forward := &http.Server{Addr: webui.ForwardAddr, Handler: ui.ForwardHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		if err := forward.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "stream-sync: remote console listener %s: %v\n", webui.ForwardAddr, err)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err = <-done:
		forward.Close()
		if err != http.ErrServerClosed {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := webui.ShutdownContext()
		defer cancel()
		go forward.Shutdown(shutdown)
		if err = server.Shutdown(shutdown); err != nil {
			return err
		}
	}
	return nil
}
