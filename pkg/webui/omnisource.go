package webui

// OmniSource (#62): with omni_source=1 the deck's streaming browser also shows
// the playlists of every mounted drive's Engine Library and of the other
// logged-in services, and search finds drive tracks next to streaming ones. A
// drive row plays the file on the drive through /v1/stream, so the DJ never
// switches sources. Engine owns those databases: they are opened read-only and
// queried only, never written.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/djay"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

const (
	defaultMediaRoot = "/media"
	localProvider    = "local"
	maxLocalResults  = 50
)

// localCodecs are the files Engine's TIDAL player takes, by manifest codec.
// shortcut: WAV/AIFF rows are left out; add them once the deck is shown to play them as a stream.
var localCodecs = map[string]string{".flac": "flac", ".mp3": "mp3", ".m4a": "mp4a.40.2", ".mp4": "mp4a.40.2", ".aac": "mp4a.40.2"}

type drive struct{ root, name, db string }

func (s *Server) omniSource() bool {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadUI().OmniSource
}

// drives lists the mounted volumes that carry an Engine Library.
func (s *Server) drives() []drive {
	root := s.mediaRoot()
	entries, _ := os.ReadDir(root)
	var out []drive
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		db := filepath.Join(dir, "Engine Library", "Database2", "m.db")
		if st, err := os.Stat(db); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			out = append(out, drive{dir, e.Name(), db})
		}
	}
	return out
}

func (s *Server) mediaRoot() string {
	if s.cfg.MediaRoot != "" {
		return s.cfg.MediaRoot
	}
	return defaultMediaRoot
}

func openDrive(d drive) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: d.db}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout(1000)&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// localTrack turns a Track row into a gateway track whose file stays on the
// drive. Rows without a playable file on the volume are left out.
func localTrack(d drive, id int64, rel, title, artist, album string, length, bpm float64) (trackRef, bool) {
	file := filepath.Join(d.root, "Engine Library", filepath.FromSlash(rel))
	if r, err := filepath.Rel(d.root, file); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return trackRef{}, false
	}
	if _, ok := localCodecs[strings.ToLower(filepath.Ext(file))]; !ok {
		return trackRef{}, false
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	}
	t := providers.Track{ID: d.db + "\x00" + strconv.FormatInt(id, 10), Title: title, Artist: artist, Album: album, DurationMS: int64(length * 1000), BPM: bpm, Provider: localProvider}
	return trackRef{track: t, file: file, drive: d.root}, true
}

// ownEntry keeps playlist entries of this drive's own tracks: they may name another library's.
const ownEntry = `(e.databaseUuid IS NULL OR e.databaseUuid='' OR e.databaseUuid=(SELECT uuid FROM Information))`

const localTrackColumns = `t.id, COALESCE(t.path,''), COALESCE(t.title,''), COALESCE(t.artist,''), COALESCE(t.album,''), COALESCE(t.length,0), COALESCE(t.bpmAnalyzed,t.bpm,0)`

type localRow struct {
	id, entity, next          int64
	rel, title, artist, album string
	length, bpm               float64
}

// scanTracks reads localTrackColumns, and with chain the entity id and nextEntityId after them.
func scanTracks(d drive, rows *sql.Rows, chain bool) ([]trackRef, []localRow, error) {
	defer rows.Close()
	var refs []trackRef
	var kept []localRow
	for rows.Next() {
		var r localRow
		dest := []any{&r.id, &r.rel, &r.title, &r.artist, &r.album, &r.length, &r.bpm}
		if chain {
			dest = append(dest, &r.entity, &r.next)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		if ref, ok := localTrack(d, r.id, r.rel, r.title, r.artist, r.album, r.length, r.bpm); ok {
			refs, kept = append(refs, ref), append(kept, r)
		}
	}
	return refs, kept, rows.Err()
}

// drivePlaylists lists a drive's playlists that hold tracks, as "[USB: <drive>] <title>".
func drivePlaylists(ctx context.Context, d drive) ([]providers.Playlist, error) {
	db, err := openDrive(d)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT p.id, COALESCE(p.title,''), count(e.id) FROM Playlist p JOIN PlaylistEntity e ON e.listId=p.id
		WHERE `+ownEntry+` GROUP BY p.id ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []providers.Playlist
	for rows.Next() {
		var id int64
		var p providers.Playlist
		if err = rows.Scan(&id, &p.Title, &p.Count); err != nil {
			return nil, err
		}
		p.ID, p.Title = d.db+"\x00"+strconv.FormatInt(id, 10), "[USB: "+d.name+"] "+p.Title
		out = append(out, p)
	}
	return out, rows.Err()
}

// driveListTracks returns a playlist's tracks in Engine's order (the
// nextEntityId chain); entries the chain does not reach keep their row order.
func (s *Server) driveListTracks(ctx context.Context, listID string) ([]trackRef, error) {
	if strings.HasPrefix(listID, "djay\x00") {
		parts := strings.Split(listID, "\x00")
		if len(parts) < 3 {
			return nil, fmt.Errorf("invalid djay playlist id")
		}
		dir := parts[1]
		plID := parts[len(parts)-1]
		libs, err := djay.Discover(dir)
		if err != nil {
			return nil, err
		}
		for _, lib := range libs {
			for _, p := range lib.Playlists {
				if p.ID == plID || p.Name == plID {
					var refs []trackRef
					for _, tr := range p.Tracks {
						if _, ok := localCodecs[strings.ToLower(filepath.Ext(tr.AbsPath))]; !ok {
							continue
						}
						trackID := tr.ID
						if trackID == "" {
							trackID = tr.RelativePath
						}
						t := providers.Track{
							ID:         "djay\x00" + dir + "\x00" + trackID,
							Title:      tr.Title,
							Artist:     tr.Artist,
							Album:      tr.Album,
							DurationMS: int64(tr.DurationSec * 1000),
							BPM:        tr.BPM,
							Key:        tr.Key,
							Provider:   localProvider,
						}
						refs = append(refs, trackRef{
							track: t,
							file:  tr.AbsPath,
							drive: dir,
						})
					}
					return refs, nil
				}
			}
		}
		return nil, fmt.Errorf("djay playlist not found: %s", plID)
	}

	dbPath, raw, _ := strings.Cut(listID, "\x00")
	var d drive
	for _, v := range s.drives() {
		if v.db == dbPath {
			d = v
		}
	}
	if d.db == "" {
		return nil, fmt.Errorf("drive no longer mounted")
	}
	db, err := openDrive(d)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT `+localTrackColumns+`, e.id, COALESCE(e.nextEntityId,0) FROM PlaylistEntity e JOIN Track t ON t.id=e.trackId
		WHERE e.listId=? AND `+ownEntry+` ORDER BY e.id`, raw)
	if err != nil {
		return nil, err
	}
	refs, kept, err := scanTracks(d, rows, true)
	if err != nil {
		return nil, err
	}
	pos, pointed := map[int64]int{}, map[int64]bool{}
	for i, r := range kept {
		pos[r.entity] = i
		pointed[r.next] = true
	}
	order := make([]int, 0, len(refs))
	seen := make([]bool, len(refs))
	for i, r := range kept {
		if pointed[r.entity] {
			continue
		}
		for j, ok := i, true; ok && !seen[j]; j, ok = pos[kept[j].next] {
			seen[j] = true
			order = append(order, j)
		}
	}
	for i := range refs {
		if !seen[i] {
			order = append(order, i)
		}
	}
	out := make([]trackRef, len(order))
	for i, j := range order {
		out[i] = refs[j]
	}
	return out, nil
}

// searchDrive finds tracks by title, artist or album on one drive.
func searchDrive(ctx context.Context, d drive, query string) ([]trackRef, error) {
	db, err := openDrive(d)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
	rows, err := db.QueryContext(ctx, `SELECT `+localTrackColumns+` FROM Track t
		WHERE t.title LIKE ?1 ESCAPE '\' OR t.artist LIKE ?1 ESCAPE '\' OR t.album LIKE ?1 ESCAPE '\' ORDER BY t.id LIMIT ?2`, like, maxLocalResults)
	if err != nil {
		return nil, err
	}
	refs, _, err := scanTracks(d, rows, false)
	return refs, err
}

// localFile resolves a drive row to its file once more when it plays: a
// symlink on the media must not lead out of the volume.
func localFile(ref trackRef) (string, error) {
	root, err := filepath.EvalSymlinks(ref.drive)
	if err != nil {
		return "", err
	}
	file, err := filepath.EvalSymlinks(ref.file)
	if err != nil {
		return "", err
	}
	if r, err := filepath.Rel(root, file); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("track outside its drive")
	}
	if st, err := os.Stat(file); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("track file missing")
	}
	return file, nil
}

// omniPlaylists adds the drives' playlists and the other services' to a listing.
func (g *tidalGateway) omniPlaylists(ctx context.Context, scope string, add func(map[string]any)) {
	drives := g.s.drives()
	for _, lists := range each(ctx, drives, drivePlaylists) {
		for _, p := range lists {
			ref := playlistRef{"", localProvider, p, false}
			add(g.refJSON(g.addPlaylist(ref), ref))
		}
	}
	root := g.s.mediaRoot()
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			dir := filepath.Join(root, e.Name())
			libs, err := djay.Discover(dir)
			if err != nil || len(libs) == 0 {
				continue
			}
			for _, lib := range libs {
				for _, p := range lib.Playlists {
					listObj := providers.Playlist{
						ID:    "djay\x00" + dir + "\x00" + p.ID,
						Title: "[djay · " + e.Name() + "] " + p.Name,
						Count: len(p.Tracks),
					}
					ref := playlistRef{"", localProvider, listObj, false}
					add(g.refJSON(g.addPlaylist(ref), ref))
				}
			}
		}
	}
	var others []*Session
	for _, v := range g.sources() {
		if v.Provider != scope {
			others = append(others, v)
		}
	}
	lists := each(ctx, others, func(ctx context.Context, v *Session) ([]providers.Playlist, error) { return v.p.Playlists(ctx) })
	for i, v := range others {
		for _, p := range lists[i] {
			p.Title = "[" + providerLabels[v.Provider] + "] " + p.Title
			ref := playlistRef{v.ID, v.Provider, p, v.Guest}
			add(g.refJSON(g.addPlaylist(ref), ref))
		}
	}
}

// omniTracks searches Algoriddim djay libraries across mounted drives for tracks matching query.
func (s *Server) omniTracks(ctx context.Context, query string) ([]trackRef, error) {
	root := s.mediaRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}
	qLower := strings.ToLower(query)
	var out []trackRef
	seenFiles := make(map[string]bool)

	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		libs, err := djay.Discover(dir)
		if err != nil || len(libs) == 0 {
			continue
		}
		for _, lib := range libs {
			for _, tr := range lib.Tracks {
				if seenFiles[tr.AbsPath] {
					continue
				}
				if _, ok := localCodecs[strings.ToLower(filepath.Ext(tr.AbsPath))]; !ok {
					continue
				}
				titleMatch := strings.Contains(strings.ToLower(tr.Title), qLower)
				artistMatch := strings.Contains(strings.ToLower(tr.Artist), qLower)
				albumMatch := strings.Contains(strings.ToLower(tr.Album), qLower)
				if !titleMatch && !artistMatch && !albumMatch {
					continue
				}
				seenFiles[tr.AbsPath] = true
				trackID := tr.ID
				if trackID == "" {
					trackID = tr.RelativePath
				}
				t := providers.Track{
					ID:         "djay\x00" + dir + "\x00" + trackID,
					Title:      tr.Title,
					Artist:     tr.Artist,
					Album:      tr.Album,
					DurationMS: int64(tr.DurationSec * 1000),
					BPM:        tr.BPM,
					Key:        tr.Key,
					Provider:   localProvider,
				}
				out = append(out, trackRef{
					track: t,
					file:  tr.AbsPath,
					drive: dir,
				})
				if len(out) >= maxLocalResults {
					return out, nil
				}
			}
		}
	}
	return out, nil
}

// omniSearch returns the drives' matches as track rows, drive by drive.
func (g *tidalGateway) omniSearch(ctx context.Context, query string) []any {
	items := []any{}
	seenFiles := make(map[string]bool)
	for _, refs := range each(ctx, g.s.drives(), func(ctx context.Context, d drive) ([]trackRef, error) { return searchDrive(ctx, d, query) }) {
		for _, ref := range refs {
			if ref.file != "" {
				seenFiles[ref.file] = true
			}
			items = append(items, g.trackJSON(g.addTrack(ref), ref.track))
		}
	}
	if djayRefs, err := g.s.omniTracks(ctx, query); err == nil {
		for _, ref := range djayRefs {
			if seenFiles[ref.file] {
				continue
			}
			seenFiles[ref.file] = true
			items = append(items, g.trackJSON(g.addTrack(ref), ref.track))
		}
	}
	return items
}
