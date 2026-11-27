package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Gateway track IDs never name two recordings (#335 rounds 9-13).
//
// Engine keeps a gateway row's numeric ID in its library (streaming://TIDAL/Track/<id>)
// and in the lists it shows, and asks for it again later, also after stream-sync
// restarted. One rule covers every ID, with no state of our own files trusted over what
// is still in use:
//
//   - nextID is the only place that hands out an ID. It hands one out only below a
//     ceiling saved first in two independently readable files, gateway-ids.json and
//     gateway-ids-2.json; a reservation counts only when both are written.
//   - loadCeiling is the only place that sets the counter. At every start it is the
//     greatest of: every valid copy of the ceiling ({"ceiling": N} and nothing else,
//     0 < N <= maxTrackID), the gateway file's counter and IDs, and every gateway ID
//     still referenced (Engine's library rows and our own files named by an ID, the
//     analysis and cover cache). The copies are rewritten when they do not both hold it.
//     So a gateway file or copies that are missing, emptied, older than the library or
//     wrong in any other way can only waste IDs, never repeat one.
//   - A value above maxTrackID is never handed out, so it bounds nothing and is ignored.
//   - While the IDs in use cannot be read no new ID is handed out; they are read again
//     until they can be (retryBound).
//
// All of this is start-up work (one read of the uri column of each library database):
// browsing and playing hand out IDs below the ceiling and write both copies once per
// idBlock IDs.

const (
	// idBlock is how many track IDs one reservation covers.
	idBlock = 1024
	// maxTrackID: Engine's TIDAL IDs stay below 2^31, like the gateway's album IDs.
	maxTrackID = 1<<31 - 1
)

// idsPaths are the two copies of the ceiling; none without a state dir.
func (g *tidalGateway) idsPaths() []string {
	if g.s == nil || g.s.cfg.StateDir == "" {
		return nil
	}
	return []string{filepath.Join(g.s.cfg.StateDir, "gateway-ids.json"), filepath.Join(g.s.cfg.StateDir, "gateway-ids-2.json")}
}

type savedCeiling struct {
	Ceiling int64 `json:"ceiling"`
}

// readCeiling is one copy's ceiling. It is an error when the copy is missing
// (fs.ErrNotExist), cannot be read, or is not valid: anything but one object
// {"ceiling": N}, or N outside 1..maxTrackID.
func readCeiling(file string) (int64, error) {
	b, err := readState(file)
	if err != nil {
		return 0, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var v struct {
		Ceiling *int64 `json:"ceiling"`
	}
	if err := dec.Decode(&v); err != nil {
		return 0, fmt.Errorf("not a ceiling (%T)", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0, errors.New("not a ceiling (more after it)")
	}
	if v.Ceiling == nil {
		return 0, errors.New("no ceiling in it")
	}
	if n := *v.Ceiling; n > 0 && n <= maxTrackID {
		return n, nil
	}
	return 0, fmt.Errorf("ceiling %d out of range", *v.Ceiling)
}

// saveCeilingCopy writes one copy atomically. Anything but a regular file at its path
// (a bad copy) is removed first, so a copy can always be repaired.
func saveCeilingCopy(file string, n int64) error {
	if st, err := os.Lstat(file); err == nil && !st.Mode().IsRegular() {
		if err := os.RemoveAll(file); err != nil {
			return err
		}
	}
	return saveState(file, savedCeiling{n})
}

// reserve saves n in both copies. It succeeds only when both are written.
func (g *tidalGateway) reserve(n int64) error {
	for _, file := range g.idsPaths() {
		if err := saveCeilingCopy(file, n); err != nil {
			return err
		}
	}
	return nil
}

// idRetry is how long retryBound waits before it reads the IDs in use again (doubling,
// up to a minute).
const idRetry = 5 * time.Second

// loadCeiling sets the counter at start-up (see the top of this file). tableMax is the
// highest in-range ID or counter the gateway file holds (0 when none or unreadable).
func (g *tidalGateway) loadCeiling(tableMax int64) {
	paths := g.idsPaths()
	if paths == nil {
		return
	}
	copies := make([]int64, len(paths))
	for i, file := range paths {
		n, err := readCeiling(file)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("stream-sync: gateway ID ceiling copy %d not used: %v", i+1, err)
		}
		copies[i] = n
	}
	g.next = max(g.next, tableMax, slices.Max(copies))
	in := idsInUse{library: idFloorLibrary, cache: rbxAnalysisDir}
	if in.library == "" {
		in.library = filepath.Join(shadowDataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library/Database2")
	}
	floor, err := in.floor(g.next)
	if err != nil {
		g.idsUnknown = true
		log.Printf("stream-sync: the gateway IDs in use cannot be read (%v): no new track IDs until they can", err)
		go g.retryBound(in, copies, idRetry)
		return
	}
	g.setBound(floor, copies)
}

// setBound starts the counter at bound, the greatest ID that can be in use, and rewrites
// the copies of the ceiling (as read: copies) unless both hold it. Caller holds g.mu,
// except at start-up.
func (g *tidalGateway) setBound(bound int64, copies []int64) {
	g.next, g.ceiling, g.idsUnknown = bound, bound, false // the first new ID reserves above it first
	if bound == 0 || !slices.ContainsFunc(copies, func(n int64) bool { return n != bound }) {
		return
	}
	if err := g.reserve(bound); err != nil {
		log.Printf("stream-sync: gateway ID ceiling copies not rewritten (%T); the next reservation writes them", err)
	}
	log.Printf("stream-sync: gateway ID ceiling set from the IDs in use: new track IDs start above %d", bound)
}

// retryBound reads the IDs in use again, after wait and then at growing intervals, until
// they can be read or the server closes: only then are new IDs handed out (#335 round
// 13: every start needs them, so a library that is busy for a moment must not cost the
// run its new rows).
func (g *tidalGateway) retryBound(in idsInUse, copies []int64, wait time.Duration) {
	for ; ; wait = min(2*wait, time.Minute) {
		select {
		case <-g.s.closed:
			return
		case <-time.After(wait):
		}
		g.mu.Lock()
		next := g.next
		g.mu.Unlock()
		if floor, err := in.floor(next); err == nil {
			g.mu.Lock()
			g.setBound(max(floor, g.next), copies)
			g.mu.Unlock()
			return
		}
	}
}

// countID is top raised to n when n can be a track ID this gateway hands out
// (1..maxTrackID); a larger value can never collide with a new ID and is ignored.
func countID(top, n int64) int64 {
	if n > 0 && n <= maxTrackID {
		return max(top, n)
	}
	return top
}

// nextID hands out a new track ID, with g.mu held, or 0 when none may be handed out
// now. Before the counter passes the ceiling, a new one is saved in both copies;
// while that fails no ID is handed out, and the next ID tries again (storage may have
// recovered). So no run hands out an ID a later run can hand out again (#335).
func (g *tidalGateway) nextID() int64 {
	if g.idsPaths() == nil { // no state at all: IDs last as long as the process
		g.next++
		return g.next
	}
	if g.idsUnknown || g.next >= maxTrackID {
		return 0
	}
	if g.next >= g.ceiling {
		ceiling := min(g.next+idBlock, maxTrackID)
		if err := g.reserve(ceiling); err != nil {
			if !g.ceilingFailing {
				log.Printf("stream-sync: gateway ID ceiling not saved (%T): no new track IDs until it is", err)
			}
			g.ceilingFailing = true
			return 0
		}
		g.ceiling, g.ceilingFailing = ceiling, false
	}
	g.next++
	return g.next
}

// idsInUse are the places outside our own ID files that still name gateway track IDs:
// Engine's library directory (Database2) and the analysis and cover cache.
type idsInUse struct{ library, cache string }

// floor is the highest gateway track ID that can still be referenced: least (what our
// own files say), Engine's library (a row whose uri is streaming://TIDAL/Track/<id>, in
// any of its databases) and the analysis and cover cache, whose files are named by an
// ID. A library database or cache that exists but cannot be read is an error.
func (in idsInUse) floor(least int64) (int64, error) {
	floor := least
	for _, name := range []string{"stm.db", "m.db", "hm.db", "sm.db"} {
		n, err := libraryMaxStreamingID(filepath.Join(in.library, name))
		if err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}
		floor = max(floor, n)
	}
	n, err := cachedMaxID(in.cache)
	if err != nil {
		return 0, fmt.Errorf("analysis cache: %w", err)
	}
	return max(floor, n), nil
}

// idFloorLibrary is Engine's library directory (Database2) the floor reads; "" is the
// deck's own, under shadowDataRoot. Only tests set it. It is read at start-up only
// (loadCeiling), never by background work.
var idFloorLibrary string

const streamingTrackURI = "streaming://TIDAL/Track/"

// libraryMaxStreamingID is the highest gateway ID one of Engine's library databases
// names (read-only; Engine stays the only writer). A missing database, or one without
// a Track uri column, names none.
func libraryMaxStreamingID(file string) (int64, error) {
	st, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !st.Mode().IsRegular() {
		return 0, errors.New("not a database file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: file}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var cols int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('Track') WHERE name = 'uri'`).Scan(&cols); err != nil {
		return 0, err
	}
	if cols == 0 {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT uri FROM Track WHERE uri LIKE 'streaming://TIDAL/Track/%'`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var top int64
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return 0, err
		}
		if len(uri) <= len(streamingTrackURI) || !strings.EqualFold(uri[:len(streamingTrackURI)], streamingTrackURI) {
			continue
		}
		if id, err := strconv.ParseInt(path.Base(uri[len(streamingTrackURI):]), 10, 64); err == nil {
			top = countID(top, id)
		}
	}
	return top, rows.Err()
}

// cachedMaxID is the highest ID in the analysis and cover cache: its files named
// <id>.bin and <id>.cover, and the ID each file of its names index holds. A names entry
// that cannot be read is removed (a cache that cannot be checked is cleared); a cache
// directory that cannot be listed is an error.
func cachedMaxID(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var top int64
	for _, e := range entries {
		for _, ext := range []string{".bin", ".cover"} {
			if base, ok := strings.CutSuffix(e.Name(), ext); ok {
				if id, err := strconv.ParseInt(base, 10, 64); err == nil {
					top = countID(top, id)
				}
			}
		}
	}
	names := filepath.Join(dir, "names")
	clear := func(p string, cause error) error {
		if err := os.RemoveAll(p); err != nil {
			return cause
		}
		return nil
	}
	sizes, err := os.ReadDir(names)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return top, nil
		}
		return top, clear(names, err)
	}
	for _, sz := range sizes {
		group := filepath.Join(names, sz.Name())
		files, err := os.ReadDir(group)
		if err != nil {
			if cerr := clear(group, err); cerr != nil {
				return 0, cerr
			}
			continue
		}
		for _, f := range files {
			p := filepath.Join(group, f.Name())
			b, err := os.ReadFile(p)
			if err != nil {
				if cerr := clear(p, err); cerr != nil {
					return 0, cerr
				}
				continue
			}
			if id, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
				top = countID(top, id)
			}
		}
	}
	return top, nil
}

// blankSecrets overwrites the credentials of the gateway file as read (raw) in place:
// in each credentialed cover URL (urls, as the file holds them) every credential value
// becomes as many 'x'. The file keeps its size, rows and IDs, and needs no new space
// (the atomic rewrite has just failed, a full disk). It is an error when a URL cannot
// be found in the file or the result would not read back as the same rows.
func blankSecrets(file string, raw []byte, urls []string) error {
	out := bytes.Clone(raw)
	slices.Sort(urls)
	for _, u := range slices.Compact(urls) {
		values := credentialValues(u)
		// Longest first: a short value inside a longer one must not leave the rest of it.
		slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
		found := false
		for _, enc := range jsonForms(u) {
			for at := 0; ; {
				i := bytes.Index(out[at:], enc)
				if i < 0 {
					break
				}
				region := out[at+i : at+i+len(enc)]
				for _, v := range values {
					for j := 0; ; {
						k := bytes.Index(region[j:], []byte(v))
						if k < 0 {
							break
						}
						copy(region[j+k:], bytes.Repeat([]byte("x"), len(v)))
						j += k + len(v)
					}
				}
				found, at = true, at+i+len(enc)
			}
		}
		if !found {
			return errors.New("a credentialed cover URL is not in the file as written")
		}
	}
	var before, after savedTracks
	if json.Unmarshal(raw, &before) != nil || json.Unmarshal(out, &after) != nil || len(before.Tracks) != len(after.Tracks) || before.Next != after.Next {
		return errors.New("the blanked file does not read back")
	}
	for i := range before.Tracks {
		if before.Tracks[i].ID != after.Tracks[i].ID {
			return errors.New("the blanked file does not read back")
		}
	}
	f, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err = f.WriteAt(out, 0); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// credentialValues are the credential parts of a cover URL as they appear in it: the
// values StripURLAuth removes (Subsonic u, t, s, p, apiKey; the Plex token) and the
// userinfo.
func credentialValues(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	var out []string
	for _, kv := range strings.Split(u.RawQuery, "&") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		switch {
		case k == "u", k == "t", k == "s", k == "p", k == "apiKey", strings.EqualFold(k, "X-Plex-Token"):
			out = append(out, v)
		}
	}
	if u.User != nil {
		out = append(out, u.User.Username())
		if p, ok := u.User.Password(); ok && p != "" {
			out = append(out, p)
		}
	}
	return slices.DeleteFunc(out, func(v string) bool { return v == "" })
}

// jsonForms are the ways a JSON writer can have put s in the file, without the quotes:
// Go's default (HTML characters escaped) and plain.
func jsonForms(s string) [][]byte {
	var forms [][]byte
	if b, err := json.Marshal(s); err == nil {
		forms = append(forms, b[1:len(b)-1])
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(s) == nil {
		b := bytes.TrimSpace(buf.Bytes())
		if plain := b[1 : len(b)-1]; len(forms) == 0 || !bytes.Equal(plain, forms[0]) {
			forms = append(forms, plain)
		}
	}
	return forms
}
