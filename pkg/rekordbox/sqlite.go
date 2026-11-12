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

func normalized(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || c == '-' || c == ' ' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

type tableSpec struct {
	quoted string
	byNorm map[string]string
}

func inspectTable(ctx context.Context, db *sql.DB, table string) (tableSpec, error) {
	q := quoteIdent(table)
	rows, err := db.QueryContext(ctx, `SELECT * FROM `+q+` LIMIT 0`)
	if err != nil {
		return tableSpec{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return tableSpec{}, err
	}
	byNorm := make(map[string]string, len(cols))
	for _, c := range cols {
		k := normalized(c)
		if _, exists := byNorm[k]; !exists {
			byNorm[k] = quoteIdent(c)
		}
	}
	return tableSpec{quoted: q, byNorm: byNorm}, rows.Err()
}

func (t tableSpec) rawExpr(names ...string) (string, bool) {
	var found []string
	for _, n := range names {
		if col, ok := t.byNorm[normalized(n)]; ok {
			found = append(found, col)
		}
	}
	switch len(found) {
	case 0:
		return "NULL", false
	case 1:
		return found[0], true
	default:
		return "COALESCE(" + strings.Join(found, ", ") + ")", true
	}
}

func (t tableSpec) valExpr(names ...string) string {
	e, _ := t.rawExpr(names...)
	return e
}

func (t tableSpec) textExpr(names ...string) string {
	var found []string
	for _, n := range names {
		if col, ok := t.byNorm[normalized(n)]; ok {
			found = append(found, "NULLIF("+col+", '')")
		}
	}
	switch len(found) {
	case 0:
		return "''"
	default:
		return "COALESCE(" + strings.Join(found, ", ") + ", '')"
	}
}

func nullToUint32(v sql.NullInt64) uint32 {
	if !v.Valid || v.Int64 <= 0 || v.Int64 > math.MaxUint32 {
		return 0
	}
	return uint32(v.Int64)
}

func parseSQLiteBPMValues(bpmx100, bpmFallback sql.NullString) float64 {
	if bpmx100.Valid && bpmx100.String != "" {
		if iv, err := strconv.ParseInt(bpmx100.String, 10, 64); err == nil {
			if iv <= 0 || iv == 0x7fffffff || iv == 0xffffffff {
				return 0
			}
			return float64(iv) / 100
		}
		fv, _ := strconv.ParseFloat(bpmx100.String, 64)
		if fv <= 0 || fv == 0x7fffffff || fv == 0xffffffff {
			return 0
		}
		return fv / 100
	}
	if !bpmFallback.Valid || bpmFallback.String == "" {
		return 0
	}
	if iv, err := strconv.ParseInt(bpmFallback.String, 10, 64); err == nil && (iv <= 0 || iv == 0x7fffffff || iv == 0xffffffff) {
		return 0
	}
	bpm, _ := strconv.ParseFloat(bpmFallback.String, 64)
	if bpm > 1000 {
		bpm /= 100
	}
	return bpm
}

func isUnsetCueTime(v int64) bool {
	return v < 0 || v == 0x7fffffff || v == 0xffffffff
}

func buildSQLiteCue(
	kind, inUsec, outUsec, inMsec, outMsec, activeVal, numVal, denVal, colorIdx sql.NullInt64,
	hasUsecCol, hasMsecCol bool,
	comment string,
) (Cue, bool) {
	if !kind.Valid || kind.Int64 < 0 || kind.Int64 > 8 {
		return Cue{}, false
	}
	var timeMS, endMS uint32
	var loop bool
	if hasUsecCol && inUsec.Valid {
		if isUnsetCueTime(inUsec.Int64) {
			return Cue{}, false
		}
		inMS := (inUsec.Int64 + 500) / 1000
		if inMS > math.MaxUint32 {
			return Cue{}, false
		}
		timeMS = uint32(inMS)
		if outUsec.Valid && outUsec.Int64 > 0 && !isUnsetCueTime(outUsec.Int64) && outUsec.Int64 > inUsec.Int64 {
			outMS := (outUsec.Int64 + 500) / 1000
			if outMS <= math.MaxUint32 {
				loop = true
				endMS = uint32(outMS)
				if endMS <= timeMS {
					endMS = timeMS + 1
				}
			}
		}
	} else if hasMsecCol && inMsec.Valid {
		if isUnsetCueTime(inMsec.Int64) || inMsec.Int64 > math.MaxUint32 {
			return Cue{}, false
		}
		timeMS = uint32(inMsec.Int64)
		if outMsec.Valid && outMsec.Int64 > 0 && !isUnsetCueTime(outMsec.Int64) && outMsec.Int64 > inMsec.Int64 && outMsec.Int64 <= math.MaxUint32 {
			loop = true
			endMS = uint32(outMsec.Int64)
		}
	} else {
		return Cue{}, false
	}

	var loopNum, loopDen uint16
	if numVal.Valid && numVal.Int64 > 0 && numVal.Int64 <= math.MaxUint16 {
		loopNum = uint16(numVal.Int64)
	}
	if denVal.Valid && denVal.Int64 > 0 && denVal.Int64 <= math.MaxUint16 {
		loopDen = uint16(denVal.Int64)
	}

	c := Cue{
		HotCue:          uint32(kind.Int64),
		TimeMS:          timeMS,
		EndMS:           endMS,
		Loop:            loop,
		Active:          loop && activeVal.Valid && activeVal.Int64 > 0,
		Name:            comment,
		LoopNumerator:   loopNum,
		LoopDenominator: loopDen,
		Extended:        true,
	}
	if colorIdx.Valid && colorIdx.Int64 > 0 {
		if kind.Int64 == 0 && colorIdx.Int64 <= 8 {
			c.ColorID = byte(colorIdx.Int64)
		} else if colorIdx.Int64 <= int64(len(rekordboxCuePalette)) {
			rgb := rekordboxCuePalette[colorIdx.Int64-1]
			c.ColorCode = byte(colorIdx.Int64)
			c.Color = [4]byte{255, rgb[0], rgb[1], rgb[2]}
		}
	}
	return c, true
}

func mergeCueSlice(cues *[]Cue, c Cue) {
	s := *cues
	for i, old := range s {
		if (c.HotCue != 0 && c.HotCue == old.HotCue) || (c.HotCue == 0 && old.HotCue == 0 && c.TimeMS == old.TimeMS && c.Loop == old.Loop && (!c.Loop || c.EndMS == old.EndMS)) {
			if c.Extended || !old.Extended {
				c.Active = c.Active || old.Active
				s[i] = c
			} else {
				s[i].Active = old.Active || c.Active
			}
			return
		}
	}
	*cues = append(s, c)
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

	findTable := func(names ...string) (tableSpec, bool, error) {
		for _, n := range names {
			if t := tables[normalized(n)]; t != "" {
				spec, err := inspectTable(ctx, db, t)
				return spec, true, err
			}
		}
		return tableSpec{}, false, nil
	}

	l := &Library{Artwork: map[uint32]string{}, Keys: map[uint32]string{}}
	lookups := map[string]map[uint32]string{}
	for _, kind := range []string{"artist", "album", "genre", "label", "key", "artwork", "image"} {
		spec, ok, err := findTable(kind, "djmd"+kind)
		if err != nil {
			return nil, err
		}
		m := map[uint32]string{}
		if ok {
			q := fmt.Sprintf("SELECT %s, %s FROM %s",
				spec.valExpr("id", kind+"_id"),
				spec.textExpr("name", "scale_name", "title", "path", "file_path"),
				spec.quoted)
			r, err := db.QueryContext(ctx, q)
			if err != nil {
				return nil, err
			}
			var id sql.NullInt64
			var val string
			for r.Next() {
				if err := r.Scan(&id, &val); err != nil {
					r.Close()
					return nil, err
				}
				m[nullToUint32(id)] = val
			}
			err = r.Err()
			r.Close()
			if err != nil {
				return nil, err
			}
		}
		lookups[kind] = m
	}
	l.Keys = lookups["key"]
	l.Artwork = lookups["artwork"]
	if len(l.Artwork) == 0 {
		l.Artwork = lookups["image"]
	}

	contentSpec, hasContent, e := findTable("content", "djmdContent", "track", "tracks")
	if e != nil {
		return nil, e
	}
	if !hasContent {
		return nil, fmt.Errorf("unsupported schema: missing content table")
	}

	var trackCount int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM `+contentSpec.quoted).Scan(&trackCount)
	if trackCount > 0 {
		l.Tracks = make([]Track, 0, trackCount)
	}

	contentQuery := fmt.Sprintf("SELECT %s FROM %s", strings.Join([]string{
		contentSpec.valExpr("id", "content_id", "track_id"),
		contentSpec.textExpr("file_path", "path", "location"),
		contentSpec.textExpr("file_name", "file_name_l", "filename"),
		contentSpec.textExpr("folder_path"),
		contentSpec.textExpr("analysisDataFilePath", "analysis_data_path", "analyze_path", "analysis_path"),
		contentSpec.textExpr("title", "name"),
		contentSpec.valExpr("artist_id_artist", "artist_id"),
		contentSpec.textExpr("artist"),
		contentSpec.valExpr("artist_id_composer", "composer_id"),
		contentSpec.valExpr("artist_id_remixer", "remixer_id"),
		contentSpec.valExpr("album_id"),
		contentSpec.textExpr("album"),
		contentSpec.valExpr("genre_id"),
		contentSpec.valExpr("label_id"),
		contentSpec.valExpr("key_id"),
		contentSpec.textExpr("key_name"),
		contentSpec.textExpr("key"),
		contentSpec.valExpr("bpmx100"),
		contentSpec.valExpr("bpm", "tempo"),
		contentSpec.valExpr("length", "duration"),
		contentSpec.valExpr("samplingRate", "sample_rate"),
		contentSpec.valExpr("bit_depth"),
		contentSpec.valExpr("bit_rate", "bitrate"),
		contentSpec.valExpr("file_size"),
		contentSpec.valExpr("release_year", "year"),
		contentSpec.valExpr("rating"),
		contentSpec.textExpr("djComment", "commnt", "comment"),
		contentSpec.valExpr("image_id"),
		contentSpec.valExpr("artwork_id"),
		contentSpec.textExpr("image_path"),
	}, ", "), contentSpec.quoted)

	cRows, e := db.QueryContext(ctx, contentQuery)
	if e != nil {
		return nil, e
	}
	var (
		cID, artistID, composerID, remixerID, albumID, genreID, labelID, keyID sql.NullInt64
		sampleRate, bitDepth, bitrate, year, rating                            sql.NullInt64
		imageID, artworkID                                                     sql.NullInt64
		bpmx100Str, bpmFallbackStr, lengthStr, fileSizeStr                     sql.NullString
		rawPath, fileName, folderPath, analysisPath, title                     string
		artistText, albumText, keyName, keyText, comment, imagePath            string
	)
	for cRows.Next() {
		if e = cRows.Scan(
			&cID, &rawPath, &fileName, &folderPath, &analysisPath, &title,
			&artistID, &artistText, &composerID, &remixerID, &albumID, &albumText,
			&genreID, &labelID, &keyID, &keyName, &keyText,
			&bpmx100Str, &bpmFallbackStr, &lengthStr,
			&sampleRate, &bitDepth, &bitrate, &fileSizeStr, &year, &rating,
			&comment, &imageID, &artworkID, &imagePath,
		); e != nil {
			cRows.Close()
			return nil, e
		}
		path := rawPath
		if path == "" && fileName != "" && folderPath != "" {
			if strings.HasSuffix(folderPath, "/"+fileName) || strings.HasSuffix(folderPath, `\`+fileName) {
				path = folderPath
			} else {
				path = strings.TrimRight(folderPath, "/") + "/" + fileName
			}
		}
		key := keyName
		if key == "" {
			key = lookups["key"][nullToUint32(keyID)]
			if key == "" {
				key = keyText
			}
		}
		imgID := nullToUint32(imageID)
		artworkPath := lookups["image"][imgID]
		if artworkPath == "" {
			if imgID != 0 {
				artworkPath = l.Artwork[imgID]
			} else {
				artworkPath = l.Artwork[nullToUint32(artworkID)]
			}
		}
		if artworkPath == "" {
			artworkPath = imagePath
		}
		var fsize int64
		if fileSizeStr.Valid && fileSizeStr.String != "" {
			if sz, err := strconv.ParseInt(fileSizeStr.String, 10, 64); err == nil && sz > 0 {
				fsize = sz
			} else if fsz, err := strconv.ParseFloat(fileSizeStr.String, 64); err == nil && fsz > 0 {
				fsize = int64(fsz)
			}
		}
		var duration float64
		if lengthStr.Valid && lengthStr.String != "" {
			duration, _ = strconv.ParseFloat(lengthStr.String, 64)
		}
		artist := lookups["artist"][nullToUint32(artistID)]
		if artist == "" {
			artist = artistText
		}
		album := lookups["album"][nullToUint32(albumID)]
		if album == "" {
			album = albumText
		}
		// OneLibrary content.length is stored in whole seconds (verified against
		// libdjinterop content_table.cpp std::chrono::seconds{*length}).
		t := Track{
			ID:           nullToUint32(cID),
			Path:         path,
			AnalysisPath: analysisPath,
			Title:        title,
			Artist:       artist,
			Composer:     lookups["artist"][nullToUint32(composerID)],
			Remixer:      lookups["artist"][nullToUint32(remixerID)],
			Album:        album,
			Genre:        lookups["genre"][nullToUint32(genreID)],
			Label:        lookups["label"][nullToUint32(labelID)],
			Key:          key,
			BPM:          parseSQLiteBPMValues(bpmx100Str, bpmFallbackStr),
			Duration:     duration,
			SampleRate:   int(nullToUint32(sampleRate)),
			BitDepth:     int(nullToUint32(bitDepth)),
			Bitrate:      int(nullToUint32(bitrate)),
			FileSize:     fsize,
			Year:         int(nullToUint32(year)),
			Rating:       int(nullToUint32(rating)),
			Comment:      comment,
			ArtworkPath:  artworkPath,
		}
		normalizeParsedAudioMetadata(&t)
		l.Tracks = append(l.Tracks, t)
	}
	e = cRows.Err()
	cRows.Close()
	if e != nil {
		return nil, e
	}

	cueSpec, hasCue, e := findTable("cue", "djmdCue")
	if e != nil {
		return nil, e
	}
	if hasCue && len(l.Tracks) > 0 {
		trackIdxByID := make(map[uint32]int, len(l.Tracks))
		for i, tr := range l.Tracks {
			if tr.ID != 0 {
				if _, dup := trackIdxByID[tr.ID]; !dup {
					trackIdxByID[tr.ID] = i
				}
			}
		}
		inUsecExpr, hasUsecCol := cueSpec.rawExpr("inUsec", "in_usec")
		inMsecExpr, hasMsecCol := cueSpec.rawExpr("inMsec", "in_msec")
		cueOrder := ""
		if idCol, ok := cueSpec.rawExpr("cue_id", "id"); ok {
			cueOrder = " ORDER BY " + idCol
		}
		cueQuery := fmt.Sprintf("SELECT %s FROM %s%s", strings.Join([]string{
			cueSpec.valExpr("content_id"),
			cueSpec.valExpr("kind"),
			inUsecExpr,
			cueSpec.valExpr("outUsec", "out_usec"),
			inMsecExpr,
			cueSpec.valExpr("outMsec", "out_msec"),
			cueSpec.valExpr("isActiveLoop", "is_active_loop", "active_loop"),
			cueSpec.valExpr("beatLoopNumerator", "beat_loop_numerator"),
			cueSpec.valExpr("beatLoopDenominator", "beat_loop_denominator"),
			cueSpec.valExpr("colorTableIndex", "color_table_index"),
			cueSpec.textExpr("cueComment", "cue_comment", "comment"),
		}, ", "), cueSpec.quoted, cueOrder)

		qRows, err := db.QueryContext(ctx, cueQuery)
		if err != nil {
			return nil, err
		}
		var (
			cueContentID, cueKind, inUsec, outUsec, inMsec, outMsec sql.NullInt64
			activeVal, numVal, denVal, colorIdx                     sql.NullInt64
			cueComment                                              string
		)
		for qRows.Next() {
			if err := qRows.Scan(
				&cueContentID, &cueKind, &inUsec, &outUsec, &inMsec, &outMsec,
				&activeVal, &numVal, &denVal, &colorIdx, &cueComment,
			); err != nil {
				qRows.Close()
				return nil, err
			}
			ti, ok := trackIdxByID[nullToUint32(cueContentID)]
			if !ok {
				continue
			}
			c, ok := buildSQLiteCue(cueKind, inUsec, outUsec, inMsec, outMsec, activeVal, numVal, denVal, colorIdx, hasUsecCol, hasMsecCol, cueComment)
			if !ok {
				continue
			}
			mergeCueSlice(&l.Tracks[ti].Cues, c)
		}
		err = qRows.Err()
		qRows.Close()
		if err != nil {
			return nil, err
		}
	}

	for i := range l.Tracks {
		l.Tracks[i].RecordHash = RecordHash(l.Tracks[i])
	}

	plSpec, hasPL, e := findTable("playlist", "djmdPlaylist")
	if e != nil {
		return nil, e
	}
	if hasPL {
		plQuery := fmt.Sprintf("SELECT %s FROM %s", strings.Join([]string{
			plSpec.valExpr("id", "playlist_id"),
			plSpec.valExpr("playlist_id_parent", "parent_id"),
			plSpec.valExpr("sequenceNo", "seq", "sort_order"),
			plSpec.textExpr("name", "title"),
			plSpec.valExpr("attribute"),
			plSpec.valExpr("is_folder"),
			plSpec.valExpr("is_smart", "intelligent"),
			plSpec.textExpr("smart_list", "rules"),
		}, ", "), plSpec.quoted)
		pRows, err := db.QueryContext(ctx, plQuery)
		if err != nil {
			return nil, err
		}
		var pID, parentID, sortOrder, attr, isFolder, isSmart sql.NullInt64
		var pName, rules string
		for pRows.Next() {
			if err := pRows.Scan(&pID, &parentID, &sortOrder, &pName, &attr, &isFolder, &isSmart, &rules); err != nil {
				pRows.Close()
				return nil, err
			}
			av := nullToUint32(attr)
			l.Playlists = append(l.Playlists, Playlist{
				ID:          nullToUint32(pID),
				ParentID:    nullToUint32(parentID),
				SortOrder:   nullToUint32(sortOrder),
				Name:        pName,
				Folder:      nullToUint32(isFolder) != 0 || av == 1,
				Intelligent: nullToUint32(isSmart) != 0 || av == 4,
				Rules:       rules,
			})
		}
		err = pRows.Err()
		pRows.Close()
		if err != nil {
			return nil, err
		}
	}

	pcSpec, hasPC, e := findTable("playlist_content", "djmdSongPlaylist", "playlist_entry", "playlist_entries")
	if e != nil {
		return nil, e
	}
	if hasPC && len(l.Playlists) > 0 {
		// Sort by source sequence before materializing intelligent and ordinary lists.
		byID := make(map[uint32]int, len(l.Playlists))
		for i, p := range l.Playlists {
			byID[p.ID] = i
		}
		knownTracks := make(map[uint32]bool, len(l.Tracks))
		for _, tr := range l.Tracks {
			knownTracks[tr.ID] = true
		}
		type entry struct{ seq, id uint32 }
		grouped := make(map[uint32][]entry, len(l.Playlists))

		pcQuery := fmt.Sprintf("SELECT %s, %s, %s FROM %s",
			pcSpec.valExpr("playlist_id"),
			pcSpec.valExpr("sequenceNo", "track_no", "seq", "entry_index", "position"),
			pcSpec.valExpr("content_id", "track_id"),
			pcSpec.quoted)
		eRows, err := db.QueryContext(ctx, pcQuery)
		if err != nil {
			return nil, err
		}
		var plID, seq, trackID sql.NullInt64
		for eRows.Next() {
			if err := eRows.Scan(&plID, &seq, &trackID); err != nil {
				eRows.Close()
				return nil, err
			}
			pid := nullToUint32(plID)
			if _, ok := byID[pid]; !ok { // orphan left by a deleted playlist
				continue
			}
			tid := nullToUint32(trackID)
			if !knownTracks[tid] {
				continue
			}
			grouped[pid] = append(grouped[pid], entry{nullToUint32(seq), tid})
		}
		err = eRows.Err()
		eRows.Close()
		if err != nil {
			return nil, err
		}
		for i, p := range l.Playlists {
			xs := grouped[p.ID]
			sort.SliceStable(xs, func(i, j int) bool { return xs[i].seq < xs[j].seq })
			if len(xs) > 0 {
				tracks := make([]uint32, len(xs))
				for j, x := range xs {
					tracks[j] = x.id
				}
				l.Playlists[i].Tracks = tracks
			}
		}
	}
	return l, nil
}
