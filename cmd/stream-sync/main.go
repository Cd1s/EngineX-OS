// stream-sync is an optional on-device playlist/download/analyze console.
package main

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

	"github.com/Cd1s/sc5000-custom-os/pkg/analyzer"
	"github.com/Cd1s/sc5000-custom-os/pkg/deemixclient"
	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
	"github.com/Cd1s/sc5000-custom-os/pkg/resourceguard"
	"github.com/Cd1s/sc5000-custom-os/pkg/webui"
)

func run() error {
	cfg := resourceguard.DefaultConfig()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	listen := flag.String("listen", ":8080", "touch/LAN HTTP listener")
	public := flag.String("public-url", "http://sc5000.lan:8080", "public console URL")
	dbPath := flag.String("database", analyzer.DefaultDatabase, "existing Engine 5.0.4 m.db (back up before use)")
	downloads := flag.String("downloads", "/media/sc5000-sd-cache/Music/Stream", "Deemix output root on the same SD volume")
	base := flag.String("deemix-url", "http://127.0.0.1:6595", "local REST bridge URL")
	queue := flag.String("deemix-queue", "/api/queue", "REST bridge queue path")
	mode := flag.String("deemix-mode", "queue", "queue REST bridge or directory (/api/download + exact ID filenames)")
	socket := flag.String("deemix-socket", "", "optional local Unix HTTP socket")
	pid := flag.Int("deemix-pid", 0, "also isolate all existing threads of this Deemix PID")
	lossy := flag.Bool("allow-320k", false, "explicitly permit verified MP3 320k fallback")
	token := flag.String("access-token", "", "console access code (default random per launch)")
	state := flag.String("state-dir", filepath.Join(home, ".config/stream-sync"), "private owner cookie state")
	spotifyToken := flag.String("spotify-token", filepath.Join(home, ".config/spotify/token.json"), "owner OAuth token file; guests never use it")
	clientID := flag.String("spotify-client-id", "", "registered Spotify public PKCE client ID")
	redirect := flag.String("spotify-redirect-url", "", "exact registered HTTPS callback URL (or device loopback for local browser)")
	netease := flag.String("netease-url", "http://127.0.0.1:3000", "local NeteaseCloudMusicApi-compatible HTTP service")
	flag.IntVar(&cfg.CPU, "cpu", 3, "dedicated background CPU")
	flag.DurationVar(&cfg.ThrottleWork, "throttle-work", cfg.ThrottleWork, "work interval before cooperative sleep")
	flag.DurationVar(&cfg.ThrottleSleep, "throttle-sleep", cfg.ThrottleSleep, "cooperative sleep interval")
	flag.BoolVar(&cfg.Disabled, "disable-guard", false, "development only: disable OS resource controls")
	flag.BoolVar(&cfg.StrictCPU, "strict-cpu", false, "refuse work when the requested isolated CPU is unavailable")
	flag.Parse()
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
	library, err := analyzer.OpenLibrary(*dbPath)
	if err != nil {
		return err
	}
	defer library.Close()
	if err = os.MkdirAll(*downloads, 0755); err != nil {
		return err
	}
	dl := &deemixclient.Client{Mode: *mode, BaseURL: *base, QueuePath: *queue, DownloadDir: *downloads, AllowLossy: *lossy}
	if *socket != "" {
		dl.HTTP = deemixclient.UnixHTTP(*socket)
	}
	jobs := webui.NewJobs(webui.Pipeline{Guard: guard, Catalog: &providers.Deezer{}, Deemix: dl, Library: library})
	defer jobs.Close()
	ui := webui.New(webui.Config{AccessToken: *token, PublicURL: *public, SpotifyClientID: *clientID, SpotifyRedirectURL: *redirect, SpotifyTokenPath: *spotifyToken, NeteaseURL: *netease, StateDir: *state}, jobs)
	// This is the one intentional local credential output: the operator needs
	// it to pair the touch/phone console. No provider credentials are logged.
	fmt.Fprintf(os.Stderr, "Stream Sync: %s\n控制台访问码: %s\n", *public, ui.AccessToken())
	server := &http.Server{Addr: *listen, Handler: ui, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
