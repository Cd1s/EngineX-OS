// engine-guardian heals Engine OS 5.0.4 libraries, or watches mounted media from
// a low-priority systemd service. --status always inspects without mutations.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/dbhealer"
)

type result struct {
	dbhealer.Report
	Error string `json:"error,omitempty"`
}

func inspect(mount string) result {
	r, err := dbhealer.InspectMount(mount)
	out := result{Report: r}
	if err != nil {
		out.Error = err.Error()
	}
	return out
}

func heal(mount string, n int) result {
	r, err := dbhealer.HealMount(mount, n)
	out := result{Report: r}
	if err != nil {
		out.Error = err.Error()
		out.Healthy = false
	}
	return out
}
func discover(root string) []string {
	var mounts []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		depth := 0
		if rel != "." {
			depth = len(strings.Split(rel, string(filepath.Separator)))
		}
		// Mount roots only, not music directories. /run/media/user/disk needs depth 2.
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
func signature(mount string) (string, error) {
	var b strings.Builder
	for _, name := range []string{"m.db", "m.db-wal", "m.db-journal", "hm.db", "hm.db-wal", "hm.db-journal"} {
		path := filepath.Join(mount, "Engine Library", "Database2", name)
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
func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("engine-guardian", flag.ContinueOnError)
	fs.SetOutput(errOut)
	mount := fs.String("heal-mount", "", "mounted volume containing Engine Library/Database2")
	watch := fs.Bool("watch", false, "watch mounted libraries; wait for quiet writes before healing")
	status := fs.Bool("status", false, "inspect libraries and print JSON without changes")
	root := fs.String("mount-root", "/media", "mount discovery root (watch/status)")
	interval := fs.Duration("interval", 10*time.Second, "watch polling interval")
	quiet := fs.Duration("quiet-for", 5*time.Second, "minimum quiet time before healing")
	sessions := fs.Int("max-sessions", dbhealer.DefaultMaxSessions, "maximum history sessions; 0 uses 50")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *interval <= 0 || *quiet < 0 || *sessions < 0 || (*watch && *status) || (*mount == "" && !*watch && !*status) {
		fmt.Fprintln(errOut, "use --heal-mount DIR, --watch, or --status; invalid option combination")
		return 2
	}
	enc := json.NewEncoder(out)
	mounts := func() []string {
		if *mount != "" {
			return []string{*mount}
		}
		return discover(*root)
	}
	if !*watch {
		results := []result{}
		code := 0
		for _, m := range mounts() {
			var r result
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
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
