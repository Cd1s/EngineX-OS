package djay

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

type CuePoint struct {
	Index      int     `json:"index"`      // 0..7 Hot Cue slot
	PositionMS float64 `json:"positionMs"` // timestamp in ms
	Label      string  `json:"label"`
	ColorHex   string  `json:"colorHex"` // e.g. "#FF0000"
}

type SavedLoop struct {
	Index    int     `json:"index"`
	StartMS  float64 `json:"startMs"`
	LengthMS float64 `json:"lengthMs"`
	Label    string  `json:"label"`
}

type Track struct {
	ID             string      `json:"id"`
	Title          string      `json:"title"`
	Artist         string      `json:"artist"`
	Album          string      `json:"album"`
	Key            string      `json:"key"`
	BPM            float64     `json:"bpm"`
	DurationSec    float64     `json:"durationSec"`
	BeatGridOffset float64     `json:"beatGridOffset"` // first beat sec
	RelativePath   string      `json:"relativePath"`
	AbsPath        string      `json:"absPath"`
	Cues           []CuePoint  `json:"cues,omitempty"`
	Loops          []SavedLoop `json:"loops,omitempty"`
}

type Playlist struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Tracks []Track `json:"tracks"`
}

type Library struct {
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	Playlists []Playlist `json:"playlists"`
	Tracks    []Track    `json:"tracks"`
}

// Discover detects Algoriddim djay libraries under mountRoot:
// - <mountRoot>/djay Media Library/djay_music_library.db
// - <mountRoot>/Algoriddim/djay_music_library.db
// - <mountRoot>/djay_music_library.db
// - <mountRoot>/djay/*.m3u8 or *.m3u
func Discover(mountRoot string) ([]Library, error) {
	if mountRoot == "" {
		return nil, nil
	}
	st, err := os.Stat(mountRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !st.IsDir() {
		return nil, nil
	}

	var libraries []Library

	// 1. Check SQLite database candidates
	dbCandidates := []string{
		filepath.Join(mountRoot, "djay Media Library", "djay_music_library.db"),
		filepath.Join(mountRoot, "Algoriddim", "djay_music_library.db"),
		filepath.Join(mountRoot, "djay_music_library.db"),
	}

	for _, dbPath := range dbCandidates {
		st, err := os.Stat(dbPath)
		if err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			lib, err := readSQLiteLibrary(mountRoot, dbPath)
			if err == nil && lib != nil {
				libraries = append(libraries, *lib)
			}
		}
	}

	// 2. Check M3U/M3U8 candidates in <mountRoot>/djay
	djayDir := filepath.Join(mountRoot, "djay")
	if dst, err := os.Stat(djayDir); err == nil && dst.IsDir() {
		m3uLib, err := readM3UDir(mountRoot, djayDir)
		if err == nil && m3uLib != nil && (len(m3uLib.Playlists) > 0 || len(m3uLib.Tracks) > 0) {
			libraries = append(libraries, *m3uLib)
		}
	}

	return libraries, nil
}

func readSQLiteLibrary(mountRoot, dbPath string) (*Library, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	u := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(abs),
		RawQuery: "mode=ro&immutable=1&_pragma=query_only(1)&_pragma=busy_timeout(1000)",
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()

	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tables := make(map[string]string)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			tables[strings.ToLower(name)] = name
		}
	}
	rows.Close()

	findTable := func(candidates ...string) string {
		for _, c := range candidates {
			if actual, ok := tables[strings.ToLower(c)]; ok {
				return actual
			}
		}
		return ""
	}

	trackTable := findTable("mediaItems", "mediaitems", "tracks", "media_items")
	cueTable := findTable("mediaItemCuePoints", "mediaitemcuepoints", "cues", "media_item_cue_points", "cue_points")
	loopTable := findTable("mediaItemLoops", "mediaitemloops", "loops", "media_item_loops", "saved_loops")
	playlistTable := findTable("mediaItemLists", "mediaitemlists", "playlists", "media_item_lists")
	playlistItemTable := findTable("mediaItemListItems", "mediaitemlistitems", "playlist_tracks", "playlist_items", "media_item_list_items")

	if trackTable == "" {
		return nil, fmt.Errorf("no tracks table found in %s", dbPath)
	}

	getCols := func(tbl string) map[string]string {
		cols := make(map[string]string)
		if tbl == "" {
			return cols
		}
		cRows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", tbl))
		if err != nil {
			return cols
		}
		defer cRows.Close()
		for cRows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			if err := cRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
				cols[strings.ToLower(name)] = name
			}
		}
		return cols
	}

	findCol := func(cols map[string]string, candidates ...string) string {
		for _, c := range candidates {
			if actual, ok := cols[strings.ToLower(c)]; ok {
				return actual
			}
		}
		return ""
	}

	colOrLit := func(col, lit string) string {
		if col != "" {
			return col
		}
		return lit
	}

	trCols := getCols(trackTable)
	idCol := findCol(trCols, "id", "rowid", "uuid", "track_id")
	if idCol == "" {
		idCol = "rowid"
	}
	pathCol := findCol(trCols, "path", "file_path", "filepath", "relative_path", "url", "location")
	titleCol := findCol(trCols, "title", "name")
	artistCol := findCol(trCols, "artist", "artist_name")
	albumCol := findCol(trCols, "album", "album_name")
	bpmCol := findCol(trCols, "bpm", "bpm_analyzed", "tempo")
	keyCol := findCol(trCols, "key", "initial_key", "musical_key", "tonality")
	durCol := findCol(trCols, "duration", "duration_sec", "length", "time")
	offsetCol := findCol(trCols, "beatgrid_offset", "beat_grid_offset", "first_beat_sec", "first_beat_offset", "grid_offset", "offset")

	trQuery := fmt.Sprintf("SELECT %s, %s, %s, %s, %s, %s, %s, %s, %s FROM %s",
		idCol,
		colOrLit(pathCol, "''"),
		colOrLit(titleCol, "''"),
		colOrLit(artistCol, "''"),
		colOrLit(albumCol, "''"),
		colOrLit(bpmCol, "0.0"),
		colOrLit(keyCol, "''"),
		colOrLit(durCol, "0.0"),
		colOrLit(offsetCol, "0.0"),
		trackTable,
	)

	tRows, err := db.QueryContext(ctx, trQuery)
	if err != nil {
		return nil, err
	}
	defer tRows.Close()

	tracksByID := make(map[string]*Track)
	var allTracks []Track

	for tRows.Next() {
		var idVal any
		var pathVal, titleVal, artistVal, albumVal, keyVal sql.NullString
		var bpmVal, durVal, offsetVal sql.NullFloat64

		if err := tRows.Scan(&idVal, &pathVal, &titleVal, &artistVal, &albumVal, &bpmVal, &keyVal, &durVal, &offsetVal); err != nil {
			return nil, err
		}

		rawID := fmt.Sprintf("%v", idVal)
		rawPath := pathVal.String
		if rawPath == "" {
			continue
		}

		absPath, relPath, ok := resolveTrackPath(mountRoot, rawPath)
		if !ok {
			continue
		}

		title := titleVal.String
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(absPath), filepath.Ext(absPath))
		}

		durationSec := durVal.Float64
		if durationSec > 1000 {
			durationSec /= 1000.0
		}

		offsetSec := offsetVal.Float64
		if offsetSec > 100 {
			offsetSec /= 1000.0
		}

		t := &Track{
			ID:             rawID,
			Title:          title,
			Artist:         artistVal.String,
			Album:          albumVal.String,
			Key:            keyVal.String,
			BPM:            bpmVal.Float64,
			DurationSec:    durationSec,
			BeatGridOffset: offsetSec,
			RelativePath:   relPath,
			AbsPath:        absPath,
		}
		tracksByID[rawID] = t
		allTracks = append(allTracks, *t)
	}
	tRows.Close()

	// Parse Cues
	cuesByTrack := make(map[string][]CuePoint)
	if cueTable != "" {
		cueCols := getCols(cueTable)
		cTrackIdCol := findCol(cueCols, "track_id", "media_item_id", "media_item_uuid", "track_uuid", "item_id")
		cSlotCol := findCol(cueCols, "slot", "index", "cue_index", "hot_cue", "number")
		cPosCol := findCol(cueCols, "position_ms", "time_ms", "timestamp_ms", "position", "time_sec", "time", "position_sec")
		cLabelCol := findCol(cueCols, "label", "name", "title")
		cColorCol := findCol(cueCols, "color", "color_hex", "colorhex")

		if cTrackIdCol != "" && cPosCol != "" {
			cueQuery := fmt.Sprintf("SELECT %s, %s, %s, %s, %s FROM %s",
				cTrackIdCol,
				colOrLit(cSlotCol, "0"),
				cPosCol,
				colOrLit(cLabelCol, "''"),
				colOrLit(cColorCol, "''"),
				cueTable,
			)
			if cSlotCol != "" {
				cueQuery += fmt.Sprintf(" ORDER BY %s", cSlotCol)
			}
			cRows, err := db.QueryContext(ctx, cueQuery)
			if err == nil {
				defer cRows.Close()
				for cRows.Next() {
					var trIDVal, slotVal any
					var posVal sql.NullFloat64
					var labelVal sql.NullString
					var colorVal any

					if err := cRows.Scan(&trIDVal, &slotVal, &posVal, &labelVal, &colorVal); err == nil {
						trID := fmt.Sprintf("%v", trIDVal)
						pos := posVal.Float64

						isSecCol := strings.Contains(strings.ToLower(cPosCol), "sec")
						isMsCol := strings.Contains(strings.ToLower(cPosCol), "ms")
						posMS := pos
						if isSecCol {
							posMS = pos * 1000.0
						} else if !isMsCol && pos < 1000 {
							if tr, ok := tracksByID[trID]; ok && tr.DurationSec > 0 && pos <= tr.DurationSec {
								posMS = pos * 1000.0
							}
						}

						slot, _ := strconv.Atoi(fmt.Sprintf("%v", slotVal))
						colorHex := parseColorHex(colorVal)

						cp := CuePoint{
							Index:      slot,
							PositionMS: posMS,
							Label:      labelVal.String,
							ColorHex:   colorHex,
						}
						cuesByTrack[trID] = append(cuesByTrack[trID], cp)
					}
				}
				cRows.Close()
			}
		}
	}

	// Parse Loops
	loopsByTrack := make(map[string][]SavedLoop)
	if loopTable != "" {
		loopCols := getCols(loopTable)
		lTrackIdCol := findCol(loopCols, "track_id", "media_item_id", "media_item_uuid", "track_uuid", "item_id")
		lSlotCol := findCol(loopCols, "slot", "index", "loop_index", "number")
		lStartCol := findCol(loopCols, "start_ms", "start_time", "start_time_ms", "start_sec", "start")
		lLenCol := findCol(loopCols, "length_ms", "length_time", "duration_ms", "length_sec", "length")
		lLabelCol := findCol(loopCols, "label", "name", "title")

		if lTrackIdCol != "" && lStartCol != "" && lLenCol != "" {
			loopQuery := fmt.Sprintf("SELECT %s, %s, %s, %s, %s FROM %s",
				lTrackIdCol,
				colOrLit(lSlotCol, "0"),
				lStartCol,
				lLenCol,
				colOrLit(lLabelCol, "''"),
				loopTable,
			)
			if lSlotCol != "" {
				loopQuery += fmt.Sprintf(" ORDER BY %s", lSlotCol)
			}
			lRows, err := db.QueryContext(ctx, loopQuery)
			if err == nil {
				defer lRows.Close()
				for lRows.Next() {
					var trIDVal, slotVal any
					var startVal, lenVal sql.NullFloat64
					var labelVal sql.NullString

					if err := lRows.Scan(&trIDVal, &slotVal, &startVal, &lenVal, &labelVal); err == nil {
						trID := fmt.Sprintf("%v", trIDVal)
						start := startVal.Float64
						length := lenVal.Float64

						isSecStart := strings.Contains(strings.ToLower(lStartCol), "sec")
						isMsStart := strings.Contains(strings.ToLower(lStartCol), "ms")
						startMS := start
						if isSecStart {
							startMS = start * 1000.0
						} else if !isMsStart && start < 1000 {
							if tr, ok := tracksByID[trID]; ok && tr.DurationSec > 0 && start <= tr.DurationSec {
								startMS = start * 1000.0
							}
						}

						isSecLen := strings.Contains(strings.ToLower(lLenCol), "sec")
						isMsLen := strings.Contains(strings.ToLower(lLenCol), "ms")
						lenMS := length
						if isSecLen {
							lenMS = length * 1000.0
						} else if !isMsLen && length < 1000 {
							if tr, ok := tracksByID[trID]; ok && tr.DurationSec > 0 && length <= tr.DurationSec {
								lenMS = length * 1000.0
							}
						}

						slot, _ := strconv.Atoi(fmt.Sprintf("%v", slotVal))
						sl := SavedLoop{
							Index:    slot,
							StartMS:  startMS,
							LengthMS: lenMS,
							Label:    labelVal.String,
						}
						loopsByTrack[trID] = append(loopsByTrack[trID], sl)
					}
				}
				lRows.Close()
			}
		}
	}

	// Attach Cues and Loops to Tracks
	for id, t := range tracksByID {
		if cues, ok := cuesByTrack[id]; ok {
			sort.Slice(cues, func(i, j int) bool { return cues[i].Index < cues[j].Index })
			t.Cues = cues
		}
		if loops, ok := loopsByTrack[id]; ok {
			sort.Slice(loops, func(i, j int) bool { return loops[i].Index < loops[j].Index })
			t.Loops = loops
		}
	}
	for i := range allTracks {
		id := allTracks[i].ID
		if tr, ok := tracksByID[id]; ok {
			allTracks[i].Cues = tr.Cues
			allTracks[i].Loops = tr.Loops
		}
	}

	// Parse Playlists
	var playlists []Playlist
	if playlistTable != "" {
		plCols := getCols(playlistTable)
		plIdCol := findCol(plCols, "id", "rowid", "uuid", "list_id")
		if plIdCol == "" {
			plIdCol = "rowid"
		}
		plNameCol := findCol(plCols, "name", "title")

		plQuery := fmt.Sprintf("SELECT %s, %s FROM %s", plIdCol, colOrLit(plNameCol, "''"), playlistTable)
		plRows, err := db.QueryContext(ctx, plQuery)
		if err == nil {
			defer plRows.Close()
			for plRows.Next() {
				var plIDVal any
				var plNameVal sql.NullString
				if err := plRows.Scan(&plIDVal, &plNameVal); err == nil {
					name := plNameVal.String
					if name == "" {
						name = fmt.Sprintf("Playlist %v", plIDVal)
					}
					playlists = append(playlists, Playlist{
						ID:   fmt.Sprintf("%v", plIDVal),
						Name: name,
					})
				}
			}
			plRows.Close()
		}
	}

	// Parse Playlist Items
	if playlistItemTable != "" && len(playlists) > 0 {
		itemCols := getCols(playlistItemTable)
		pIdCol := findCol(itemCols, "playlist_id", "list_id", "media_item_list_id")
		tIdCol := findCol(itemCols, "track_id", "media_item_id", "item_id")
		orderCol := findCol(itemCols, "position", "sort_order", "item_order", "rank", "index", "id", "rowid")

		if pIdCol != "" && tIdCol != "" {
			itemQuery := fmt.Sprintf("SELECT %s, %s FROM %s", pIdCol, tIdCol, playlistItemTable)
			if orderCol != "" {
				itemQuery += fmt.Sprintf(" ORDER BY %s", orderCol)
			}
			iRows, err := db.QueryContext(ctx, itemQuery)
			if err == nil {
				defer iRows.Close()
				tracksByPlaylist := make(map[string][]Track)
				for iRows.Next() {
					var pVal, tVal any
					if err := iRows.Scan(&pVal, &tVal); err == nil {
						pID := fmt.Sprintf("%v", pVal)
						tID := fmt.Sprintf("%v", tVal)
						if tr, ok := tracksByID[tID]; ok && tr != nil {
							tracksByPlaylist[pID] = append(tracksByPlaylist[pID], *tr)
						}
					}
				}
				iRows.Close()

				for i := range playlists {
					playlists[i].Tracks = tracksByPlaylist[playlists[i].ID]
				}
			}
		}
	}

	lib := &Library{
		Name:      filepath.Base(filepath.Dir(dbPath)),
		Path:      dbPath,
		Playlists: playlists,
		Tracks:    allTracks,
	}
	return lib, nil
}

func readM3UDir(mountRoot, djayDir string) (*Library, error) {
	entries, err := os.ReadDir(djayDir)
	if err != nil {
		return nil, err
	}

	var playlists []Playlist
	trackMap := make(map[string]Track)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".m3u8" && ext != ".m3u" {
			continue
		}

		m3uPath := filepath.Join(djayDir, e.Name())
		pl, err := parseM3UFile(mountRoot, m3uPath)
		if err != nil || pl == nil {
			continue
		}
		playlists = append(playlists, *pl)
		for _, tr := range pl.Tracks {
			trackMap[tr.AbsPath] = tr
		}
	}

	if len(playlists) == 0 {
		return nil, nil
	}

	var allTracks []Track
	for _, tr := range trackMap {
		allTracks = append(allTracks, tr)
	}
	sort.Slice(allTracks, func(i, j int) bool { return allTracks[i].Title < allTracks[j].Title })

	return &Library{
		Name:      "djay",
		Path:      djayDir,
		Playlists: playlists,
		Tracks:    allTracks,
	}, nil
}

func parseM3UFile(mountRoot, m3uPath string) (*Playlist, error) {
	f, err := os.Open(m3uPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	plName := strings.TrimSuffix(filepath.Base(m3uPath), filepath.Ext(m3uPath))
	playlist := &Playlist{
		ID:   filepath.Base(m3uPath),
		Name: plName,
	}

	var curTitle, curArtist, curKey string
	var curDur, curBPM, curOffset float64
	var curCues []CuePoint
	var curLoops []SavedLoop

	resetTrackMeta := func() {
		curTitle = ""
		curArtist = ""
		curKey = ""
		curDur = 0
		curBPM = 0
		curOffset = 0
		curCues = nil
		curLoops = nil
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "#EXTM3U" {
			continue
		}

		if strings.HasPrefix(line, "#EXTINF:") {
			rest := strings.TrimPrefix(line, "#EXTINF:")
			durStr, titlePart, ok := strings.Cut(rest, ",")
			if ok {
				dur, _ := strconv.ParseFloat(strings.TrimSpace(durStr), 64)
				curDur = dur
				titlePart = strings.TrimSpace(titlePart)
				if artist, title, hasDash := strings.Cut(titlePart, " - "); hasDash {
					curArtist = strings.TrimSpace(artist)
					curTitle = strings.TrimSpace(title)
				} else {
					curTitle = titlePart
				}
			}
			continue
		}

		if strings.HasPrefix(line, "#EXTDJAY:") {
			extStr := strings.TrimPrefix(line, "#EXTDJAY:")
			bpm, key, offset, cues, loops := parseExtDjay(extStr)
			if bpm > 0 {
				curBPM = bpm
			}
			if key != "" {
				curKey = key
			}
			if offset != 0 {
				curOffset = offset
			}
			if len(cues) > 0 {
				curCues = cues
			}
			if len(loops) > 0 {
				curLoops = loops
			}
			continue
		}

		if strings.HasPrefix(line, "#") {
			continue
		}

		// Audio track path
		rawAudioPath := line
		candPath := rawAudioPath
		if !filepath.IsAbs(rawAudioPath) {
			candPath = filepath.Join(filepath.Dir(m3uPath), rawAudioPath)
		}
		absPath, relPath, ok := resolveTrackPath(mountRoot, candPath)
		if !ok {
			absPath, relPath, ok = resolveTrackPath(mountRoot, rawAudioPath)
		}

		if ok {
			title := curTitle
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(absPath), filepath.Ext(absPath))
			}
			tr := Track{
				ID:             filepath.ToSlash(relPath),
				Title:          title,
				Artist:         curArtist,
				DurationSec:    curDur,
				BPM:            curBPM,
				Key:            curKey,
				BeatGridOffset: curOffset,
				RelativePath:   relPath,
				AbsPath:        absPath,
				Cues:           curCues,
				Loops:          curLoops,
			}
			playlist.Tracks = append(playlist.Tracks, tr)
		}

		resetTrackMeta()
	}

	return playlist, scanner.Err()
}

func parseExtDjay(line string) (bpm float64, key string, offset float64, cues []CuePoint, loops []SavedLoop) {
	parts := strings.Split(line, ",")
	for _, part := range parts {
		k, v, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "BPM":
			bpm, _ = strconv.ParseFloat(v, 64)
		case "KEY":
			key = v
		case "BEATGRID_OFFSET", "OFFSET", "BEATGRID":
			offset, _ = strconv.ParseFloat(v, 64)
		case "CUES":
			cues = parseM3UCues(v)
		case "LOOPS":
			loops = parseM3ULoops(v)
		}
	}
	return
}

func parseM3UCues(v string) []CuePoint {
	var cues []CuePoint
	items := strings.Split(v, ";")
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, ":")
		var cp CuePoint
		if len(parts) >= 1 {
			cp.Index, _ = strconv.Atoi(parts[0])
		}
		if len(parts) >= 2 {
			cp.PositionMS, _ = strconv.ParseFloat(parts[1], 64)
		}
		if len(parts) >= 3 {
			cp.Label = parts[2]
		}
		if len(parts) >= 4 {
			cp.ColorHex = parseColorHex(parts[3])
		}
		cues = append(cues, cp)
	}
	return cues
}

func parseM3ULoops(v string) []SavedLoop {
	var loops []SavedLoop
	items := strings.Split(v, ";")
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, ":")
		var sl SavedLoop
		if len(parts) >= 1 {
			sl.Index, _ = strconv.Atoi(parts[0])
		}
		if len(parts) >= 2 {
			sl.StartMS, _ = strconv.ParseFloat(parts[1], 64)
		}
		if len(parts) >= 3 {
			sl.LengthMS, _ = strconv.ParseFloat(parts[2], 64)
		}
		if len(parts) >= 4 {
			sl.Label = parts[3]
		}
		loops = append(loops, sl)
	}
	return loops
}

func resolveTrackPath(mountRoot, rawPath string) (string, string, bool) {
	if rawPath == "" {
		return "", "", false
	}

	if strings.HasPrefix(rawPath, "file://") {
		if u, err := url.Parse(rawPath); err == nil {
			rawPath = u.Path
			if unescaped, err := url.PathUnescape(rawPath); err == nil {
				rawPath = unescaped
			}
		} else {
			rawPath = strings.TrimPrefix(rawPath, "file://")
		}
	}

	rawPath = filepath.Clean(filepath.FromSlash(rawPath))
	mountClean := filepath.Clean(mountRoot)

	var candidates []string

	if filepath.IsAbs(rawPath) {
		candidates = append(candidates, rawPath)

		parts := strings.Split(filepath.ToSlash(rawPath), "/")
		if len(parts) >= 3 && (parts[1] == "Volumes" || parts[1] == "media" || parts[1] == "mnt") {
			relFromDrive := strings.Join(parts[3:], "/")
			candidates = append(candidates, filepath.Join(mountClean, filepath.FromSlash(relFromDrive)))
		}
	} else {
		candidates = append(candidates, filepath.Join(mountClean, rawPath))
	}

	evalMount, err := filepath.EvalSymlinks(mountClean)
	if err != nil {
		evalMount = mountClean
	}

	for _, cand := range candidates {
		candClean := filepath.Clean(cand)

		rel, err := filepath.Rel(mountClean, candClean)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}

		st, err := os.Stat(candClean)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}

		evalCand, err := filepath.EvalSymlinks(candClean)
		if err != nil {
			continue
		}
		relEval, err := filepath.Rel(evalMount, evalCand)
		if err != nil || relEval == ".." || strings.HasPrefix(relEval, ".."+string(filepath.Separator)) {
			continue
		}

		stEval, err := os.Stat(evalCand)
		if err != nil || !stEval.Mode().IsRegular() {
			continue
		}

		return candClean, filepath.ToSlash(rel), true
	}

	return "", "", false
}

func parseColorHex(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		val = strings.TrimSpace(val)
		if val == "" {
			return ""
		}
		if strings.HasPrefix(val, "#") {
			return strings.ToUpper(val)
		}
		if len(val) == 6 {
			return "#" + strings.ToUpper(val)
		}
		if n, err := strconv.ParseInt(val, 10, 64); err == nil && n != 0 {
			return fmt.Sprintf("#%06X", n&0xFFFFFF)
		}
		return val
	case int64:
		if val != 0 {
			return fmt.Sprintf("#%06X", val&0xFFFFFF)
		}
	case int:
		if val != 0 {
			return fmt.Sprintf("#%06X", val&0xFFFFFF)
		}
	case float64:
		n := int64(val)
		if n != 0 {
			return fmt.Sprintf("#%06X", n&0xFFFFFF)
		}
	}
	return ""
}
