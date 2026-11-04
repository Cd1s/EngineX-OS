package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
)

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
		case *status:
			r, err = dbhealer.InspectMount(m)
		case *dataOnly:
			r, err = dbhealer.HealMountDataOnly(m, *sessions)
		default:
			r, err = dbhealer.HealMount(m, *sessions)
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
