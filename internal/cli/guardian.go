package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/webui"
)

var guardianBootReport = webui.WriteBootReport

type guardianResult struct {
	dbhealer.Report
	MP3Grids *enginedb.MP3GridReport `json:"mp3_grids,omitempty"`
	Error    string                  `json:"error,omitempty"`
}

func mp3GridBudget(deadline time.Duration, mountOnly bool) time.Duration {
	const defaultMountBudget = 4 * time.Second
	switch {
	case deadline > 0:
		b := deadline / 2
		if b > defaultMountBudget {
			b = defaultMountBudget
		}
		if b <= 0 {
			b = deadline
		}
		return b
	case mountOnly:
		return defaultMountBudget
	default:
		return 0
	}
}

const guardianCleanupReserve = 3 * time.Second

func guardianRemaining(start time.Time, deadline time.Duration) time.Duration {
	if deadline <= 0 {
		return 0
	}
	return time.Until(start.Add(deadline))
}

// guardianLockWait is how long a run waits for a volume's lock: two seconds, or what is left of
// its --timeout when that is less.
func guardianLockWait(start time.Time, deadline time.Duration) time.Duration {
	wait := 2 * time.Second
	if deadline > 0 {
		wait = max(0, min(wait, guardianRemaining(start, deadline)))
	}
	return wait
}

// Guardian heals or inspects Engine OS 5.0.4 libraries once and prints JSON.
//
// --watch is retired for the reason rb-bridge's is: healing a library Engine
// has open races Engine's own writes. Repairs run from the mount hook, before
// Engine attaches. Old units still pass --watch, so it exits at once.
func Guardian(args []string, out, errOut io.Writer) int {
	commandStart := time.Now()
	fs := flag.NewFlagSet("engine-guardian", flag.ContinueOnError)
	fs.SetOutput(errOut)
	mount := fs.String("heal-mount", "", "mounted volume containing Engine Library/Database2")
	status := fs.Bool("status", false, "inspect libraries and print JSON without changes")
	root := fs.String("mount-root", "/media", "mount discovery root when no --heal-mount is given")
	dataOnly := fs.Bool("data-only", false, "repair table data but leave Engine's views, triggers and indexes untouched")
	sessions := fs.Int("max-sessions", dbhealer.DefaultMaxSessions, "maximum history sessions; 0 uses 50")
	reserve := fs.Bool("normalize-reserve", false, "only rebuild libraries whose page reserve byte is not 0 (breaks ATTACH in Engine 5.3.0); Engine must not be running")
	fixGrids := fs.Bool("fix-mp3-grids", false, "once-per-library shift of pre-5.3.0.4 EngineX MP3 beat grids, cues and loops (standalone unless combined with --data-only or --normalize-reserve)")
	bootReport := fs.Bool("boot-report", false, "save and export a sanitized boot report without touching libraries")
	uiDir := fs.String("ui-dir", "/data/enginex/ui", "boot safety state directory")
	deadline := fs.Duration("timeout", 0, "maximum command runtime; 0 disables the deadline")
	checkCopy := fs.String("check-copy", "", "check a library the mount hook copied onto a stick before it becomes its Engine Library; exit 1 when refused")
	watch := fs.Bool("watch", false, "retired; exits at once")
	fs.Duration("interval", 0, "retired")
	fs.Duration("quiet-for", 0, "retired")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *watch {
		fmt.Fprintln(errOut, "engine-guardian: --watch is retired; the mount hook repairs each volume before Engine opens it")
		return 0
	}
	if *deadline < 0 {
		fmt.Fprintln(errOut, "timeout must not be negative")
		return 2
	}
	var hardTimer *time.Timer
	if *deadline > 0 {
		// Vendor images need not ship a timeout executable or BusyBox applet.
		hardBudget := *deadline
		if *dataOnly {
			hardBudget += guardianCleanupReserve + time.Second
		}
		// Data-only callers need a hard-stop cushion so the hook deadline wins
		// the race. Other callers keep their exact historical timeout.
		hardTimer = time.AfterFunc(time.Until(commandStart.Add(hardBudget)), func() { os.Exit(124) })
		defer hardTimer.Stop()
	}
	if *checkCopy != "" {
		if fs.NArg() != 0 || *mount != "" || *status || *dataOnly || *reserve || *fixGrids || *bootReport {
			fmt.Fprintln(errOut, "--check-copy cannot be combined with other operations")
			return 2
		}
		r, err := dbhealer.CheckCopy(*checkCopy)
		code := 0
		if err != nil {
			fmt.Fprintln(errOut, "engine-guardian: copied library refused:", err)
			code = 1
		}
		if e := json.NewEncoder(out).Encode(r); e != nil {
			fmt.Fprintln(errOut, e)
			return 1
		}
		return code
	}
	if *bootReport {
		if fs.NArg() != 0 || *mount != "" || *status || *dataOnly || *reserve || *fixGrids {
			fmt.Fprintln(errOut, "--boot-report cannot be combined with database operations")
			return 2
		}
		if err := guardianBootReport(*uiDir, *root); err != nil {
			// No captured output or private paths reach stderr. Fallback must continue.
			fmt.Fprintln(errOut, "engine-guardian: boot report could not be saved or exported")
			return 1
		}
		return 0
	}
	if fs.NArg() != 0 || *sessions < 0 || (*mount == "" && !*status) {
		fmt.Fprintln(errOut, "use --heal-mount DIR or --status; invalid option combination")
		return 2
	}
	mounts := []string{*mount}
	if *mount == "" {
		mounts = discoverLibraries(*root)
	}
	results := []guardianResult{}
	code := 0
	for _, m := range mounts {
		var r dbhealer.Report
		var mp3Report *enginedb.MP3GridReport
		var err error
		// Every mode but --status changes the library. The volume's lock is taken once for the
		// mount and handed to each step, so no other EngineX program writes between them. Busy
		// after the wait: nothing is touched, the run fails and the next mount tries again.
		hold, release := context.Background(), func() {}
		var lockErr error
		if !*status {
			if hold, release, lockErr = rbstate.HoldVolume(hold, m, guardianLockWait(commandStart, *deadline)); lockErr != nil {
				hold, release = context.Background(), func() {}
			}
		}
		runMP3Fix := func(budget time.Duration, mountOnly bool) {
			if budget < 0 {
				return
			}
			mr, ferr := enginedb.FixMP3Grids(hold, m, enginedb.MP3GridOptions{
				Budget: mp3GridBudget(budget, mountOnly),
			})
			mp3Report = &mr
			if ferr != nil {
				fmt.Fprintln(errOut, "engine-guardian: mp3 grid fix:", ferr)
			} else if mr.Corrected > 0 || mr.Skipped > 0 {
				fmt.Fprintf(errOut, "engine-guardian: mp3 grid fix: corrected=%d unchanged=%d skipped=%d interrupted=%v\n",
					mr.Corrected, mr.Unchanged, mr.Skipped, mr.Interrupted)
			}
		}
		switch {
		case lockErr != nil:
			r.Mount, r.CheckedAt, err = m, time.Now().UTC(), fmt.Errorf("lock volume %s: %w", m, lockErr)
		case *status: // read-only, so it wins over --reserve
			r, err = dbhealer.InspectMount(m)
		case *reserve:
			r.Mount, r.CheckedAt = m, time.Now().UTC()
			r.ReserveFixed, err = dbhealer.NormalizeMountReserveContext(hold, m)
			r.Healthy = err == nil
			if err == nil && *fixGrids {
				runMP3Fix(guardianRemaining(commandStart, *deadline), false)
			}
		case *fixGrids && !*dataOnly:
			r.Mount, r.CheckedAt = m, time.Now().UTC()
			mr, ferr := enginedb.FixMP3Grids(hold, m, enginedb.MP3GridOptions{
				Budget: mp3GridBudget(*deadline, false),
			})
			mp3Report = &mr
			err = ferr
			r.Healthy = err == nil
		default:
			// Edits the Web Console queued while Engine had this library open
			// (Relocate, Hot Cues): this runs from the mount hook before Engine
			// opens it again.
			if enginedb.LibraryInUse(m) {
				r.Mount, r.CheckedAt, err = m, time.Now().UTC(), enginedb.ErrLibraryInUse
				break
			}
			if n, perr := enginedb.ApplyPending(hold, m); perr != nil {
				if errors.Is(perr, enginedb.ErrLibraryInUse) {
					r.Mount, r.CheckedAt, err = m, time.Now().UTC(), perr
					break
				}
				fmt.Fprintln(errOut, "engine-guardian: queued library edits not applied:", perr)
			} else if n > 0 {
				fmt.Fprintf(errOut, "engine-guardian: applied %d queued library edits\n", n)
			}
			mountCtx := hold
			cancelMount := func() {}
			if *deadline > 0 {
				remaining := guardianRemaining(commandStart, *deadline)
				if remaining < 0 {
					remaining = 0
				}
				mountCtx, cancelMount = context.WithTimeout(hold, remaining)
			}
			budgetExpired := false
			opts := dbhealer.MountOptions{
				MaxSessions:                   *sessions,
				DataOnly:                      *dataOnly,
				ExtractEmbeddedArtworkContext: enginedb.ExtractEmbeddedArtworkContext,
			}
			if *dataOnly {
				r, err = dbhealer.HealMountWithOptions(mountCtx, m, opts)
				budgetExpired = mountCtx.Err() != nil
			} else {
				r, err = dbhealer.HealMountWithOptions(hold, m, opts)
			}
			cancelMount()
			if err == nil && !budgetExpired {
				remaining := guardianRemaining(commandStart, *deadline)
				if *deadline == 0 || remaining >= 500*time.Millisecond {
					runMP3Fix(remaining, *dataOnly)
				}
			}
		}
		if err == nil && !*status {
			if progressErr := webui.RecordBootDatabaseProgress(*uiDir); progressErr != nil {
				// Progress reporting must never change the database operation's result.
				fmt.Fprintln(errOut, "engine-guardian: boot progress could not be recorded")
			}
		}
		res := guardianResult{Report: r, MP3Grids: mp3Report}
		if err != nil {
			res.Error, res.Healthy, code = err.Error(), false, 1
		}
		results = append(results, res)
		release()
	}
	if err := json.NewEncoder(out).Encode(results); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	return code
}

// discoverLibraries looks at most two levels under root (/run/media/user/disk),
// never into music directories.
func discoverLibraries(root string) []string {
	var mounts []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if rel, _ := filepath.Rel(root, path); rel != "." && len(strings.Split(rel, string(filepath.Separator))) > 2 {
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
