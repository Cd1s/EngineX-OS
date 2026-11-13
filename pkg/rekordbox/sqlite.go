package rekordbox

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// Public rekordbox 62-entry cue color-code palette (indices 1..62, documented by
// Deep Symmetry / Beat Link CueList.findRekordboxColor).
var rekordboxCuePalette = [62][3]byte{
	{0x30, 0x5a, 0xff}, {0x50, 0x73, 0xff}, {0x50, 0x8c, 0xff}, {0x50, 0xa0, 0xff},
	{0x50, 0xb4, 0xff}, {0x50, 0xb0, 0xf2}, {0x50, 0xae, 0xe8}, {0x45, 0xac, 0xdb},
	{0x00, 0xe0, 0xff}, {0x19, 0xda, 0xf0}, {0x32, 0xd2, 0xe6}, {0x21, 0xb4, 0xb9},
	{0x20, 0xaa, 0xa0}, {0x1f, 0xa3, 0x92}, {0x19, 0xa0, 0x8c}, {0x14, 0xa5, 0x84},
	{0x14, 0xaa, 0x7d}, {0x10, 0xb1, 0x76}, {0x30, 0xd2, 0x6e}, {0x37, 0xde, 0x5a},
	{0x3c, 0xeb, 0x50}, {0x28, 0xe2, 0x14}, {0x7d, 0xc1, 0x3d}, {0x8c, 0xc8, 0x32},
	{0x9b, 0xd7, 0x23}, {0xa5, 0xe1, 0x16}, {0xa5, 0xdc, 0x0a}, {0xaa, 0xd2, 0x08},
	{0xb4, 0xc8, 0x05}, {0xb4, 0xbe, 0x04}, {0xba, 0xb4, 0x04}, {0xc3, 0xaf, 0x04},
	{0xe1, 0xaa, 0x00}, {0xff, 0xa0, 0x00}, {0xff, 0x96, 0x00}, {0xff, 0x8c, 0x00},
	{0xff, 0x75, 0x00}, {0xe0, 0x64, 0x1b}, {0xe0, 0x46, 0x1e}, {0xe0, 0x30, 0x1e},
	{0xe0, 0x28, 0x23}, {0xe6, 0x28, 0x28}, {0xff, 0x37, 0x6f}, {0xff, 0x2d, 0x6f},
	{0xff, 0x12, 0x7b}, {0xf5, 0x1e, 0x8c}, {0xeb, 0x2d, 0xa0}, {0xe6, 0x37, 0xb4},
	{0xde, 0x44, 0xcf}, {0xde, 0x44, 0x8d}, {0xe6, 0x30, 0xb4}, {0xe6, 0x19, 0xdc},
	{0xe6, 0x00, 0xff}, {0xdc, 0x00, 0xff}, {0xcc, 0x00, 0xff}, {0xb4, 0x32, 0xff},
	{0xb9, 0x3c, 0xff}, {0xc5, 0x42, 0xff}, {0xaa, 0x5a, 0xff}, {0xaa, 0x72, 0xff},
	{0x82, 0x72, 0xff}, {0x64, 0x73, 0xff},
}

// SQLiteAdapter permits vendor-version-specific OneLibrary readers without
// coupling the sync engine to a particular SQLite export schema or decryption.
type SQLiteAdapter interface {
	Read(context.Context, *sql.DB) (*Library, error)
}
type OneLibraryAdapter struct{}

func ReadSQLite(ctx context.Context, path string, adapter SQLiteAdapter) (*Library, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	// mode=ro alone can still create/write a WAL shared-memory sidecar. Export
	// files must be clean snapshots: immutable avoids all source locks/writes,
	// and refusing journals prevents silently ignoring uncheckpointed changes.
	for _, suffix := range []string{"-wal", "-journal"} {
		st, err := os.Lstat(abs + suffix)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil && (!st.Mode().IsRegular() || st.Size() > 0) {
			return nil, fmt.Errorf("OneLibrary: requires checkpointed export; source sidecar %s is present", suffix)
		}
	}
	openPath := abs
	encrypted, e := isEncryptedSQLite(abs)
	if e != nil {
		return nil, e
	}
	if encrypted {
		tmpPath, err := decryptSQLCipher4ToTemp(ctx, abs, []byte(oneLibraryPassphrase))
		if err != nil {
			return nil, fmt.Errorf("OneLibrary SQLCipher: %w", err)
		}
		defer os.Remove(tmpPath)
		openPath = tmpPath
	}
	u := url.URL{Scheme: "file", Path: openPath}
	db, e := sql.Open("sqlite", u.String()+"?mode=ro&immutable=1&_pragma=query_only(1)")
	if e != nil {
		return nil, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if adapter == nil {
		adapter = OneLibraryAdapter{}
	}
	l, e := adapter.Read(ctx, db)
	if e != nil {
		return nil, fmt.Errorf("OneLibrary SQLite: %w", e)
	}
	if e = l.Validate(); e != nil {
		return nil, e
	}
	return l, nil
}

type sqlRecord map[string]string

// Built once: a Replacer per call allocated a lookup table for every column
// name of every row (GBs of garbage for a 20000-track cue table).
var nameSeparators = strings.NewReplacer("_", "", "-", "", " ", "")

func normalized(s string) string {
	return strings.ToLower(nameSeparators.Replace(s))
}
func (r sqlRecord) get(names ...string) string {
	for _, n := range names {
		if v := r[normalized(n)]; v != "" {
			return v
		}
	}
	return ""
}
func (r sqlRecord) num(names ...string) uint32 {
	v, _ := strconv.ParseUint(r.get(names...), 10, 32)
	return uint32(v)
}
func (r sqlRecord) sint(names ...string) (int64, bool) {
	s := r.get(names...)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
func (r sqlRecord) decimal(names ...string) float64 {
	v, _ := strconv.ParseFloat(r.get(names...), 64)
	return v
}

func parseSQLiteBPM(r sqlRecord) float64 {
	if rawStr := r.get("bpmx100"); rawStr != "" {
		if iv, err := strconv.ParseInt(rawStr, 10, 64); err == nil {
			if iv <= 0 || iv == 0x7fffffff || iv == 0xffffffff {
				return 0
			}
			return float64(iv) / 100
		}
		fv := r.decimal("bpmx100")
		if fv <= 0 || fv == 0x7fffffff || fv == 0xffffffff {
			return 0
		}
		return fv / 100
	}
	if iv, ok := r.sint("bpm", "tempo"); ok && (iv <= 0 || iv == 0x7fffffff || iv == 0xffffffff) {
		return 0
	}
	bpm := r.decimal("bpm", "tempo")
	if bpm > 1000 {
		bpm /= 100
	}
	return bpm
}

func isUnsetCueTime(v int64) bool {
	return v < 0 || v == 0x7fffffff || v == 0xffffffff
}

func parseSQLiteCue(r sqlRecord) (Cue, bool) {
	kind, ok := r.sint("kind")
	if !ok || kind < 0 || kind > 8 {
		return Cue{}, false
	}
	var timeMS, endMS uint32
	var loop bool
	if inUsec, hasUsec := r.sint("inUsec", "in_usec"); hasUsec {
		if isUnsetCueTime(inUsec) {
			return Cue{}, false
		}
		inMS := (inUsec + 500) / 1000
		if inMS > math.MaxUint32 {
			return Cue{}, false
		}
		timeMS = uint32(inMS)
		if outUsec, hasOut := r.sint("outUsec", "out_usec"); hasOut && outUsec > 0 && !isUnsetCueTime(outUsec) && outUsec > inUsec {
			outMS := (outUsec + 500) / 1000
			if outMS <= math.MaxUint32 {
				loop = true
				endMS = uint32(outMS)
				if endMS <= timeMS {
					endMS = timeMS + 1
				}
			}
		}
	} else if inMsec, hasMsec := r.sint("inMsec", "in_msec"); hasMsec {
		if isUnsetCueTime(inMsec) || inMsec > math.MaxUint32 {
			return Cue{}, false
		}
		timeMS = uint32(inMsec)
		if outMsec, hasOut := r.sint("outMsec", "out_msec"); hasOut && outMsec > 0 && !isUnsetCueTime(outMsec) && outMsec > inMsec && outMsec <= math.MaxUint32 {
			loop = true
			endMS = uint32(outMsec)
		}
	} else {
		return Cue{}, false
	}

	activeVal, _ := r.sint("isActiveLoop", "is_active_loop", "active_loop")
	numVal, _ := r.sint("beatLoopNumerator", "beat_loop_numerator")
	denVal, _ := r.sint("beatLoopDenominator", "beat_loop_denominator")
	var loopNum, loopDen uint16
	if numVal > 0 && numVal <= math.MaxUint16 {
		loopNum = uint16(numVal)
	}
	if denVal > 0 && denVal <= math.MaxUint16 {
		loopDen = uint16(denVal)
	}

	c := Cue{
		HotCue:          uint32(kind),
		TimeMS:          timeMS,
		EndMS:           endMS,
		Loop:            loop,
		Active:          loop && activeVal > 0,
		Name:            r.get("cueComment", "cue_comment", "comment"),
		LoopNumerator:   loopNum,
		LoopDenominator: loopDen,
		Extended:        true,
	}
	if colorIdx, hasColor := r.sint("colorTableIndex", "color_table_index"); hasColor && colorIdx > 0 {
		if kind == 0 && colorIdx <= 8 {
			c.ColorID = byte(colorIdx)
		} else if colorIdx <= int64(len(rekordboxCuePalette)) {
			rgb := rekordboxCuePalette[colorIdx-1]
			c.ColorCode = byte(colorIdx)
			c.Color = [4]byte{255, rgb[0], rgb[1], rgb[2]}
		}
	}
	return c, true
}

var (
	// cueColumns are the cue table columns parseSQLiteCue and the cue import use.
	cueColumns = []string{"content_id", "cue_id", "id", "kind", "inUsec", "in_usec", "outUsec", "out_usec", "inMsec", "in_msec", "outMsec", "out_msec",
		"isActiveLoop", "is_active_loop", "active_loop", "beatLoopNumerator", "beat_loop_numerator", "beatLoopDenominator", "beat_loop_denominator",
		"cueComment", "cue_comment", "comment", "colorTableIndex", "color_table_index"}

	contentColumns = []string{
		"id", "content_id", "track_id",
		"file_path", "path", "location", "file_name", "file_name_l", "filename", "folder_path",
		"bpmx100", "bpm", "tempo",
		"key_name", "key_id", "key",
		"image_id", "artwork_id", "image_path",
		"file_size",
		"analysisDataFilePath", "analysis_data_path", "analyze_path", "analysis_path",
		"title", "name",
		"artist_id_artist", "artist_id", "artist",
		"artist_id_composer", "composer_id",
		"artist_id_remixer", "remixer_id",
		"album_id", "album",
		"genre_id", "label_id",
		"length", "duration",
		"samplingRate", "sample_rate",
		"bit_depth", "bit_rate", "bitrate",
		"release_year", "year", "rating",
		"djComment", "commnt", "comment",
	}

	playlistColumns = []string{
		"id", "playlist_id", "playlist_id_parent", "parent_id",
		"sequenceNo", "seq", "sort_order",
		"name", "title",
		"attribute", "is_folder", "is_smart", "intelligent",
		"smart_list", "rules",
	}

	playlistContentColumns = []string{
		"playlist_id",
		"sequenceNo", "track_no", "seq", "entry_index", "position",
		"content_id", "track_id",
	}
)

// eachSQLRow streams the wanted columns of table through fn, one reused record
// at a time. Loading every cue as a map of all its columns took 375 MB on armhf
// for 20000 tracks with 8 cues each (r2 review).
func eachSQLRow(ctx context.Context, db *sql.DB, table string, wanted []string, fn func(sqlRecord)) error {
	want := map[string]bool{}
	for _, w := range wanted {
		want[normalized(w)] = true
	}
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	info, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	var cols, keys []string
	var delCol string
	for info.Next() {
		var name string
		if err = info.Scan(&name); err != nil {
			info.Close()
			return err
		}
		if normalized(name) == "rblocaldeleted" {
			delCol = quote(name)
		}
		if want[normalized(name)] {
			cols, keys = append(cols, quote(name)), append(keys, normalized(name))
		}
	}
	err = info.Err()
	info.Close()
	if err != nil || len(cols) == 0 {
		return err
	}
	where := ""
	if delCol != "" {
		where = ` WHERE COALESCE(` + delCol + `, 0) = 0`
	}
	rows, err := db.QueryContext(ctx, `SELECT `+strings.Join(cols, ",")+` FROM `+quote(table)+where)
	if err != nil {
		return err
	}
	defer rows.Close()
	values := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range ptrs {
		ptrs[i] = &values[i]
	}
	r := sqlRecord{}
	for rows.Next() {
		if err = rows.Scan(ptrs...); err != nil {
			return err
		}
		clear(r)
		for i, v := range values {
			if v != nil {
				r[keys[i]] = string(v)
			}
		}
		fn(r)
	}
	return rows.Err()
}

func (OneLibraryAdapter) Read(ctx context.Context, db *sql.DB) (*Library, error) {
	rows, e := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if e != nil {
		return nil, e
	}
	tables := map[string]string{}
	for rows.Next() {
		var s string
		if e = rows.Scan(&s); e != nil {
			rows.Close()
			return nil, e
		}
		tables[normalized(s)] = s
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	findTable := func(names ...string) string {
		for _, n := range names {
			if t := tables[normalized(n)]; t != "" {
				return t
			}
		}
		return ""
	}
	l := &Library{Artwork: map[uint32]string{}, Keys: map[uint32]string{}}
	lookups := map[string]map[uint32]string{}
	for _, kind := range []string{"artist", "album", "genre", "label", "key", "artwork", "image"} {
		m := map[uint32]string{}
		if tbl := findTable(kind, "djmd"+kind); tbl != "" {
			if e := eachSQLRow(ctx, db, tbl, []string{"id", kind + "_id", "name", "scale_name", "title", "path", "file_path"}, func(r sqlRecord) {
				m[r.num("id", kind+"_id")] = r.get("name", "scale_name", "title", "path", "file_path")
			}); e != nil {
				return nil, e
			}
		}
		lookups[kind] = m
	}
	l.Keys = lookups["key"]
	l.Artwork = lookups["artwork"]
	if len(l.Artwork) == 0 {
		l.Artwork = lookups["image"]
	}
	contentTable := findTable("content", "djmdContent", "track", "tracks")
	if contentTable == "" {
		return nil, fmt.Errorf("unsupported schema: missing content table")
	}
	if e := eachSQLRow(ctx, db, contentTable, contentColumns, func(r sqlRecord) {
		path := r.get("file_path", "path", "location")
		if filename := r.get("file_name", "file_name_l", "filename"); path == "" && filename != "" {
			if folder := r.get("folder_path"); folder != "" {
				if strings.HasSuffix(folder, "/"+filename) || strings.HasSuffix(folder, `\`+filename) {
					path = folder
				} else {
					path = strings.TrimRight(folder, "/") + "/" + filename
				}
			}
		}
		bpm := parseSQLiteBPM(r)
		key := r.get("key_name")
		if key == "" {
			key = lookups["key"][r.num("key_id")]
			if key == "" {
				key = r.get("key")
			}
		}
		artworkPath := lookups["image"][r.num("image_id")]
		if artworkPath == "" {
			artworkPath = l.Artwork[r.num("image_id", "artwork_id")]
		}
		if artworkPath == "" {
			artworkPath = r.get("image_path")
		}
		var fileSize int64
		if sz, ok := r.sint("file_size"); ok && sz > 0 {
			fileSize = sz
		} else if fsz := r.decimal("file_size"); fsz > 0 {
			fileSize = int64(fsz)
		}
		// OneLibrary content.length is stored in whole seconds (verified against
		// libdjinterop content_table.cpp std::chrono::seconds{*length}).
		t := Track{ID: r.num("id", "content_id", "track_id"), Path: path, AnalysisPath: r.get("analysisDataFilePath", "analysis_data_path", "analyze_path", "analysis_path"), Title: r.get("title", "name"), Artist: lookups["artist"][r.num("artist_id_artist", "artist_id")], Composer: lookups["artist"][r.num("artist_id_composer", "composer_id")], Remixer: lookups["artist"][r.num("artist_id_remixer", "remixer_id")], Album: lookups["album"][r.num("album_id")], Genre: lookups["genre"][r.num("genre_id")], Label: lookups["label"][r.num("label_id")], Key: key, BPM: bpm, Duration: r.decimal("length", "duration"), SampleRate: int(r.num("samplingRate", "sample_rate")), BitDepth: int(r.num("bit_depth")), Bitrate: int(r.num("bit_rate", "bitrate")), FileSize: fileSize, Year: int(r.num("release_year", "year")), Rating: int(r.num("rating")), Comment: r.get("djComment", "commnt", "comment"), ArtworkPath: artworkPath}
		if t.Artist == "" {
			t.Artist = r.get("artist")
		}
		if t.Album == "" {
			t.Album = r.get("album")
		}
		normalizeParsedAudioMetadata(&t)
		l.Tracks = append(l.Tracks, t)
	}); e != nil {
		return nil, e
	}
	cueTable := findTable("cue", "djmdCue")
	if cueTable != "" {
		type indexedCue struct {
			id  int64
			seq int
			cue Cue
		}
		byContent := map[uint32][]indexedCue{}
		idx := -1
		if e = eachSQLRow(ctx, db, cueTable, cueColumns, func(r sqlRecord) {
			idx++
			cid := r.num("content_id")
			if cid == 0 {
				return
			}
			c, ok := parseSQLiteCue(r)
			if !ok {
				return
			}
			cueID, _ := r.sint("cue_id", "id")
			byContent[cid] = append(byContent[cid], indexedCue{id: cueID, seq: idx, cue: c})
		}); e != nil {
			return nil, e
		}
		for i := range l.Tracks {
			items := byContent[l.Tracks[i].ID]
			if len(items) == 0 {
				continue
			}
			sort.SliceStable(items, func(a, b int) bool {
				if items[a].id != items[b].id {
					return items[a].id < items[b].id
				}
				return items[a].seq < items[b].seq
			})
			var merged Analysis
			for _, item := range items {
				merged.mergeCue(item.cue)
			}
			l.Tracks[i].Cues = merged.Cues
		}
	}
	for i := range l.Tracks {
		l.Tracks[i].RecordHash = RecordHash(l.Tracks[i])
	}
	if plTable := findTable("playlist", "djmdPlaylist"); plTable != "" {
		if e := eachSQLRow(ctx, db, plTable, playlistColumns, func(r sqlRecord) {
			attr := r.num("attribute")
			l.Playlists = append(l.Playlists, Playlist{ID: r.num("id", "playlist_id"), ParentID: r.num("playlist_id_parent", "parent_id"), SortOrder: r.num("sequenceNo", "seq", "sort_order"), Name: r.get("name", "title"), Folder: r.num("is_folder") != 0 || attr == 1, Intelligent: r.num("is_smart", "intelligent") != 0 || attr == 4, Rules: r.get("smart_list", "rules")})
		}); e != nil {
			return nil, e
		}
	}
	// Sort by source sequence before materializing intelligent and ordinary lists.
	byID := map[uint32]int{}
	for i, p := range l.Playlists {
		byID[p.ID] = i
	}
	type entry struct{ seq, id uint32 }
	grouped := map[uint32][]entry{}
	if pcTable := findTable("playlist_content", "djmdSongPlaylist", "playlist_entry", "playlist_entries"); pcTable != "" {
		if e := eachSQLRow(ctx, db, pcTable, playlistContentColumns, func(r sqlRecord) {
			id := r.num("playlist_id")
			if _, ok := byID[id]; !ok { // orphan left by a deleted playlist
				return
			}
			grouped[id] = append(grouped[id], entry{r.num("sequenceNo", "track_no", "seq", "entry_index", "position"), r.num("content_id", "track_id")})
		}); e != nil {
			return nil, e
		}
	}
	knownTracks := make(map[uint32]bool, len(l.Tracks))
	for _, tr := range l.Tracks {
		knownTracks[tr.ID] = true
	}
	for i, p := range l.Playlists {
		xs := grouped[p.ID]
		sort.SliceStable(xs, func(i, j int) bool { return xs[i].seq < xs[j].seq })
		for _, x := range xs {
			if knownTracks[x.id] {
				l.Playlists[i].Tracks = append(l.Playlists[i].Tracks, x.id)
			}
		}
	}
	return l, nil
}
