package enginedb

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// PlanUpdate reports, writing nothing, what an import of l into the library in dir would read:
// reread are the imported tracks (Engine paths) whose files or export record changed or were never
// read ("fast:"), added the tracks the library does not have yet. It stats every track's files
// and reads MP3 and M4A headers, as a full import does, but reads only the bridge state, never
// Engine's m.db, which Engine may have open: an update with little to read needs no copy of the
// library to read it in (#253).
func PlanUpdate(ctx context.Context, res *compat.Resolver, l *rekordbox.Library, dir string) (reread []string, added int, err error) {
	if err = l.Validate(); err != nil {
		return nil, 0, err
	}
	stored := map[string]string{}
	state := filepath.Join(dir, stateFile)
	if _, err = os.Stat(state); err == nil {
		db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: state, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String())
		if err != nil {
			return nil, 0, err
		}
		defer db.Close()
		rows, err := db.QueryContext(ctx, `SELECT path,fingerprint FROM BridgeState`)
		if err != nil {
			return nil, 0, err
		}
		defer rows.Close()
		for rows.Next() {
			var p, fp string
			if err = rows.Scan(&p, &fp); err != nil {
				return nil, 0, err
			}
			stored[p] = fp
		}
		if err = rows.Err(); err != nil {
			return nil, 0, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	prepared, err := prepareTracks(ctx, res, l, time.Time{}, time.Now, nil)
	if err != nil {
		return nil, 0, err
	}
	for _, p := range prepared {
		switch fp, ok := stored["../"+p.rel]; {
		case !ok:
			added++
		case fp != p.hash:
			reread = append(reread, "../"+p.rel)
		}
	}
	return reread, added, nil
}
