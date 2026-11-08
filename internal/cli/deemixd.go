package cli

import (
	"context"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	deemixdAddr = "127.0.0.1:6595"
	deemixdURL  = "http://" + deemixdAddr
)

// Waits between checks and restarts; tests shorten them.
var (
	deemixdPause      = 3 * time.Second
	deemixdMaxBackoff = time.Minute
	deemixdMissing    = 30 * time.Second
	// minimal-club-mode writes this to pause the downloader; a var so tests can point it elsewhere.
	deemixdPausedFlag = "/data/enginex/mods-state/minimal-club-mode/paused.json"
)

// sameDeemixd reports whether base names the address sc5000-deemixd binds.
func sameDeemixd(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Scheme == "http" && (u.Host == deemixdAddr || u.Host == "localhost:6595")
}

func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err == nil {
		c.Close()
	}
	return err == nil
}

// superviseDeemixd keeps a downloader answering on addr until ctx ends. It
// starts bin only while nothing answers there, so a copy started by systemd or
// by hand is left alone. The child inherits stream-sync's cgroup, CPU affinity
// and nice level, and systemd stops it together with stream-sync.
func superviseDeemixd(ctx context.Context, addr, bin string, args ...string) {
	backoff := deemixdPause
	reported := false
	for {
		wait := deemixdPause
		if _, err := os.Stat(deemixdPausedFlag); err == nil {
			wait = deemixdPause // minimal-club-mode paused the downloader: do not respawn it
		} else if !listening(addr) {
			if _, err := os.Stat(bin); err != nil {
				if !reported {
					log.Printf("stream-sync: no downloader: %v", err)
					reported = true
				}
				wait = deemixdMissing
			} else {
				reported = false
				started := time.Now()
				err := runDeemixd(ctx, bin, args)
				if ctx.Err() != nil {
					return
				}
				if listening(addr) {
					// systemd's sc5000-deemixd bound 127.0.0.1:6595 while this child started (#159).
					backoff = deemixdPause
					wait = deemixdPause
				} else {
					// A crash loop must not spin; a long run earns a quick restart.
					if time.Since(started) > 30*time.Second {
						backoff = deemixdPause
					} else if backoff *= 2; backoff > deemixdMaxBackoff {
						backoff = deemixdMaxBackoff
					}
					wait = backoff
					log.Printf("stream-sync: sc5000-deemixd exited (%v); checking again in %s", err, wait)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func runDeemixd(ctx context.Context, bin string, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// SIGTERM lets it remove its partial downloads before exiting.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 12 * time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("stream-sync: started sc5000-deemixd (pid %d)", cmd.Process.Pid)
	return cmd.Wait()
}
