package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/webui"
)

var guardianBootReport = webui.WriteBootReport

type guardianResult struct {
	dbhealer.Report
	Error string `json:"error,omitempty"`
}

// Guardian heals or inspects Engine OS 5.0.4 libraries once and prints JSON.
//
// --watch is retired for the reason rb-bridge's is: healing a library Engine
// has open races Engine's own writes. Repairs run from the mount hook, before
// Engine attaches. Old units still pass --watch, so it exits at once.
func Guardian(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("engine-guardian", flag.ContinueOnError)
	fs.SetOutput(errOut)
	mount := fs.String("heal-mount", "", "mounted volume containing Engine Library/Database2")
	status := fs.Bool("status", false, "inspect libraries and print JSON without changes")
	root := fs.String("mount-root", "/media", "mount discovery root when no --heal-mount is given")
	dataOnly := fs.Bool("data-only", false, "repair table data but leave Engine's views, triggers and indexes untouched")
	sessions := fs.Int("max-sessions", dbhealer.DefaultMaxSessions, "maximum history sessions; 0 uses 50")
	reserve := fs.Bool("normalize-reserve", false, "only rebuild libraries whose page reserve byte is not 0 (breaks ATTACH in Engine 5.3.0); Engine must not be running")
	bootReport := fs.Bool("boot-report", false, "save and export a sanitized boot report without touching libraries")
	uiDir := fs.String("ui-dir", "/data/enginex/ui", "boot safety state directory")
	deadline := fs.Duration("timeout", 0, "maximum command runtime; 0 disables the deadline")
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
	if *deadline > 0 {
		// Vendor images need not ship a timeout executable or BusyBox applet.
		// This CLI-only deadline also bounds a locked repair before Engine starts.
		timer := time.AfterFunc(*deadline, func() { os.Exit(124) })
		defer timer.Stop()
	}
	if *bootReport {
		if fs.NArg() != 0 || *mount != "" || *status || *dataOnly || *reserve {
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
		var err error
		switch {
		case *status: // read-only, so it wins over --reserve
			r, err = dbhealer.InspectMount(m)
		case *reserve:
			r.Mount, r.CheckedAt = m, time.Now().UTC()
			r.ReserveFixed, err = dbhealer.NormalizeDatabase2(filepath.Join(m, "Engine Library", "Database2"))
			r.Healthy = err == nil
		default:
			// Edits the Web Console queued while Engine had this library open
			// (Relocate, Hot Cues): this runs from the mount hook before Engine
			// opens it again.
			if n, perr := enginedb.ApplyPending(context.Background(), m); perr != nil {
				fmt.Fprintln(errOut, "engine-guardian: queued library edits not applied:", perr)
			} else if n > 0 {
				fmt.Fprintf(errOut, "engine-guardian: applied %d queued library edits\n", n)
			}
			if *dataOnly {
				r, err = dbhealer.HealMountDataOnly(m, *sessions)
			} else {
				r, err = dbhealer.HealMount(m, *sessions)
			}
		}
		if err == nil && !*status {
			if progressErr := webui.RecordBootDatabaseProgress(*uiDir); progressErr != nil {
				// Progress reporting must never change the database operation's result.
				fmt.Fprintln(errOut, "engine-guardian: boot progress could not be recorded")
			}
		}
		res := guardianResult{Report: r}
		if err != nil {
			res.Error, res.Healthy, code = err.Error(), false, 1
		}
		results = append(results, res)
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
