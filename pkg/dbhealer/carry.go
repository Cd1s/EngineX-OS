package dbhealer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CopyReport is what CheckCopy found in a copied library.
type CopyReport struct {
	Dir     string `json:"dir"`
	Tracks  int    `json:"tracks"`
	Missing int    `json:"missing"`
}

// CheckCopy checks a library the mount hook copied to dir, a folder in a drive's root that is
// renamed "Engine Library" next if this passes. This is the carry-over of the library Engine built
// in /data for an HFS+ stick while it was read-only. m.db must pass integrity_check. Every local
// track's path, which Engine keeps relative to its library folder ("../Music/a.mp3", see
// relocate.go), must name a place on this drive, the way findMissing resolves it. A library with
// local tracks of which none is found here is refused too: its paths were not made for this drive.
// A missing file alone is no reason to refuse, because it was just as missing in the read-only view.
func CheckCopy(dir string) (CopyReport, error) {
	r := CopyReport{Dir: dir}
	db, err := Open(filepath.Join(dir, "Database2", "m.db"), false) // writable: a WAL copied along is replayed into the copy
	if err != nil {
		return r, err
	}
	defer db.Close()
	var check string
	if err = db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil {
		return r, err
	}
	if check != "ok" {
		return r, fmt.Errorf("database integrity: %s", check)
	}
	rows, err := db.Query(`SELECT COALESCE(path,'') FROM Track WHERE COALESCE(streamingSource,'')='' AND COALESCE(path,'')<>''`)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	root := filepath.Dir(dir)
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			return r, err
		}
		file := filepath.Join(dir, filepath.FromSlash(p))
		rel, e := filepath.Rel(root, file)
		if filepath.IsAbs(p) || e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return r, fmt.Errorf("track path %q is not on this drive", p)
		}
		r.Tracks++
		if _, e := os.Stat(file); e != nil {
			r.Missing++
		}
	}
	if err = rows.Err(); err != nil {
		return r, err
	}
	if r.Tracks > 0 && r.Missing == r.Tracks {
		return r, fmt.Errorf("none of the %d tracks is on this drive", r.Tracks)
	}
	return r, nil
}
