package rekordbox

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"io"
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
	return ReadSQLiteKeys(ctx, path, adapter, nil)
}

// ReadSQLiteKeys is ReadSQLite with the SQLCipher keys of kc (#321).
func ReadSQLiteKeys(ctx context.Context, path string, adapter SQLiteAdapter, kc *KeyCache) (*Library, error) {
	return readSQLite(ctx, path, adapter, kc, false)
}

// ReadSQLiteWhole is ReadSQLite of a file that must be whole (#321 review R4): its pages,
// decrypted for SQLCipher, are a whole number, the count its header gives when the header's count
// is valid, and SQLite's quick_check finds nothing missing or damaged. ReadSQLite reads only the
// tables it needs, so a copy cut inside its last page, or by whole pages where the header leaves
// the count to the file's size, still reads.
func ReadSQLiteWhole(ctx context.Context, path string) (*Library, error) {
	return readSQLite(ctx, path, nil, nil, true)
}

// ReadSQLiteWholeKeys is ReadSQLiteWhole using kc for the SQLCipher key cache.
func ReadSQLiteWholeKeys(ctx context.Context, path string, kc *KeyCache) (*Library, error) {
	return readSQLite(ctx, path, nil, kc, true)
}

func readSQLite(ctx context.Context, path string, adapter SQLiteAdapter, kc *KeyCache, whole bool) (library *Library, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%s: %w", path, err)
		}
	}()
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
		tmpPath, err := decryptSQLCipher4ToTempKeys(ctx, abs, []byte(oneLibraryPassphrase), kc)
		if err != nil {
			return nil, fmt.Errorf("OneLibrary SQLCipher: %w", err)
		}
		defer os.Remove(tmpPath)
		openPath = tmpPath
	}
	if whole {
		if e = sqlitePagesWhole(openPath); e != nil {
			return nil, e
		}
	}
	u := url.URL{Scheme: "file", Path: openPath}
	db, e := sql.Open("sqlite", u.String()+"?mode=ro&immutable=1&_pragma=query_only(1)")
	if e != nil {
		return nil, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if whole {
		var check string
		if e = db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); e != nil || check != "ok" {
			return nil, fmt.Errorf("OneLibrary SQLite: quick_check: %q %v", check, e)
		}
	}
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

// sqlDriverRecord is a projected row backed by the database/sql driver's
// reusable Value slice. Reading through driver.Rows avoids database/sql's
// per-cell Scan conversion and keeps numeric fields in their native type.
type sqlDriverRecord struct {
	values []driver.Value
	index  map[string]int
}

// Built once: a Replacer per call allocated a lookup table for every column
// name of every row (GBs of garbage for a 20000-track cue table).
var nameSeparators = strings.NewReplacer("_", "", "-", "", " ", "")

func normalized(s string) string {
	// Export column and table names are ASCII. Keep the common path allocation-light
	// because get() runs once for every projected field of every row.
	var buf [64]byte
	out := buf[:0]
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			return strings.ToLower(nameSeparators.Replace(s))
		}
		if c == '_' || c == '-' || c == ' ' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
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

func (r sqlDriverRecord) raw(names ...string) driver.Value {
	for _, n := range names {
		if i, ok := r.index[n]; ok && r.values[i] != nil {
			return r.values[i]
		}
		if i, ok := r.index[normalized(n)]; ok && r.values[i] != nil {
			return r.values[i]
		}
	}
	return nil
}

func driverString(v driver.Value) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return ""
	}
}

func rawUint(v []byte) (uint64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	var n uint64
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := uint64(c - '0')
		if n > (math.MaxUint64-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}

func rawInt(v []byte) (int64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	neg := false
	if v[0] == '-' {
		neg = true
		v = v[1:]
	} else if v[0] == '+' {
		v = v[1:]
	}
	n, ok := rawUint(v)
	if !ok || (!neg && n > math.MaxInt64) || (neg && n > uint64(math.MaxInt64)+1) {
		return 0, false
	}
	if neg {
		if n == uint64(math.MaxInt64)+1 {
			return math.MinInt64, true
		}
		return -int64(n), true
	}
	return int64(n), true
}

func driverInt(v driver.Value) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x), true
		}
	case float64:
		if x >= -0x1p63 && x < 0x1p63 && x == math.Trunc(x) {
			return int64(x), true
		}
	case []byte:
		return rawInt(x)
	case string:
		return rawInt([]byte(x))
	}
	return 0, false
}

func (r sqlDriverRecord) get(names ...string) string {
	return driverString(r.raw(names...))
}

func (r sqlDriverRecord) num(names ...string) uint32 {
	n, ok := driverInt(r.raw(names...))
	if !ok || n < 0 || n > math.MaxUint32 {
		return 0
	}
	return uint32(n)
}

func (r sqlDriverRecord) sint(names ...string) (int64, bool) {
	return driverInt(r.raw(names...))
}

func (r sqlDriverRecord) decimal(names ...string) float64 {
	return driverDecimal(r.raw(names...))
}

type sqlRecordLike interface {
	get(...string) string
	num(...string) uint32
	sint(...string) (int64, bool)
	decimal(...string) float64
}

func parseSQLiteBPM(r sqlRecordLike) float64 {
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

func parseSQLiteCue(r sqlRecordLike) (Cue, bool) {
	kind, ok := r.sint("kind")
	if !ok || kind < 0 || kind > 8 {
		return Cue{}, false
	}
	var timeMS, endMS uint32
	var loop bool
	if inUsec, hasUsec := r.sint("inUsec", "in_usec"); hasUsec {
		if isUnsetCueTime(inUsec) || inUsec > math.MaxInt64-500 {
			return Cue{}, false
		}
		inMS := (inUsec + 500) / 1000
		if inMS > math.MaxUint32 {
			return Cue{}, false
		}
		timeMS = uint32(inMS)
		if outUsec, hasOut := r.sint("outUsec", "out_usec"); hasOut && outUsec > 0 && !isUnsetCueTime(outUsec) && outUsec > inUsec && outUsec <= math.MaxInt64-500 {
			outMS := (outUsec + 500) / 1000
			if outMS <= math.MaxUint32 {
				endMS = uint32(outMS)
				if endMS <= timeMS {
					if timeMS < math.MaxUint32 {
						loop = true
						endMS = timeMS + 1
					} else {
						endMS = 0
					}
				} else {
					loop = true
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
	contentColumnGroups = []sqlColumnGroup{
		{"id", "content_id", "track_id"},
		{"file_path", "path", "location"},
		{"file_name", "file_name_l", "filename"},
		{"folder_path"},
		{"bpmx100"},
		{"bpm", "tempo"},
		{"key_name"},
		{"key_id"},
		{"key"},
		{"image_id"},
		{"artwork_id"},
		{"image_path"},
		{"file_size"},
		{"analysisDataFilePath", "analysis_data_path", "analyze_path", "analysis_path"},
		{"title", "name"},
		{"artist_id_artist", "artist_id"},
		{"artist_id_composer", "composer_id"},
		{"artist_id_remixer", "remixer_id"},
		{"album_id"},
		{"album"},
		{"genre_id"},
		{"label_id"},
		{"length", "duration"},
		{"samplingRate", "sample_rate"},
		{"bit_depth"},
		{"bit_rate", "bitrate"},
		{"release_year", "year"},
		{"rating"},
		{"djComment", "commnt", "comment"},
		{"artist"},
	}
	cueColumnGroups = []sqlColumnGroup{
		{"content_id"},
		{"cue_id", "id"},
		{"kind"},
		{"inUsec", "in_usec"},
		{"outUsec", "out_usec"},
		{"inMsec", "in_msec"},
		{"outMsec", "out_msec"},
		{"isActiveLoop", "is_active_loop", "active_loop"},
		{"beatLoopNumerator", "beat_loop_numerator"},
		{"beatLoopDenominator", "beat_loop_denominator"},
		{"cueComment", "cue_comment", "comment"},
		{"colorTableIndex", "color_table_index"},
	}
	playlistColumnGroups = []sqlColumnGroup{
		{"id", "playlist_id"},
		{"playlist_id_parent", "parent_id"},
		{"sequenceNo", "seq", "sort_order"},
		{"name", "title"},
		{"attribute"},
		{"is_folder"},
		{"is_smart", "intelligent"},
		{"smart_list", "rules"},
	}
	playlistContentColumnGroups = []sqlColumnGroup{
		{"playlist_id"},
		{"sequenceNo", "track_no", "seq", "entry_index", "position"},
		{"content_id", "track_id"},
	}
)

func lookupColumnGroups(kind string) []sqlColumnGroup {
	return []sqlColumnGroup{
		{"id", kind + "_id"},
		{"name", "scale_name", "title", "path", "file_path"},
	}
}

type sqlColumnGroup []string

type contentProjection struct {
	sql                                    sqlProjection
	id                                     int
	path                                   int
	fileName, folderPath                   int
	bpmX100, bpm                           int
	keyName, keyID, key                    int
	imageID, artworkID, imagePath          int
	fileSize, analysisPath                 int
	title, artistID, composerID, remixerID int
	albumID, album, genreID, labelID       int
	length, sampleRate, bitDepth, bitRate  int
	releaseYear, rating, comment, artist   int
}

type sqlProjection struct {
	selects []string
	index   map[string]int
	where   string
	table   string
}

func buildSQLProjection(ctx context.Context, db *sql.DB, table string, groups []sqlColumnGroup) (sqlProjection, error) {
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	info, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return sqlProjection{}, err
	}
	columns := map[string]string{}
	var delCol string
	for info.Next() {
		var name string
		if err = info.Scan(&name); err != nil {
			info.Close()
			return sqlProjection{}, err
		}
		normal := normalized(name)
		columns[normal] = name
		if normal == "rblocaldeleted" {
			delCol = quote(name)
		}
	}
	err = info.Err()
	info.Close()
	if err != nil {
		return sqlProjection{}, err
	}

	p := sqlProjection{index: make(map[string]int, len(groups)*2), table: table}
	for _, group := range groups {
		actual := make([]string, 0, len(group))
		for _, name := range group {
			if column := columns[normalized(name)]; column != "" {
				actual = append(actual, column)
			}
		}
		if len(actual) == 0 {
			continue
		}
		expr := quote(actual[0])
		if len(actual) > 1 {
			parts := make([]string, len(actual))
			for i, column := range actual {
				parts[i] = `NULLIF(` + quote(column) + `, '')`
			}
			expr = `COALESCE(` + strings.Join(parts, ",") + `)`
		}
		p.selects = append(p.selects, expr+` AS `+quote(group[0]))
		at := len(p.selects) - 1
		for _, name := range group {
			p.index[normalized(name)] = at
			p.index[name] = at
		}
	}
	if delCol != "" {
		p.where = ` WHERE COALESCE(` + delCol + `, 0) = 0`
	}
	return p, nil
}

func (p sqlProjection) each(ctx context.Context, db *sql.DB, fn func([]driver.Value)) error {
	if len(p.selects) == 0 {
		return nil
	}
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(raw any) error {
		q, ok := raw.(driver.QueryerContext)
		if !ok {
			return fmt.Errorf("sqlite driver does not support direct row queries")
		}
		rows, err := q.QueryContext(ctx, `SELECT `+strings.Join(p.selects, ",")+` FROM `+quote(p.table)+p.where, nil)
		if err != nil {
			return err
		}
		defer rows.Close()
		values := make([]driver.Value, len(p.selects))
		for {
			if err := rows.Next(values); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			fn(values)
		}
	})
}

func eachSQLProjectedRow(ctx context.Context, db *sql.DB, table string, groups []sqlColumnGroup, fn func(sqlDriverRecord)) error {
	p, err := buildSQLProjection(ctx, db, table, groups)
	if err != nil {
		return err
	}
	return p.each(ctx, db, func(values []driver.Value) {
		fn(sqlDriverRecord{values: values, index: p.index})
	})
}

func contentField(p sqlProjection, names ...string) int {
	for _, name := range names {
		if i, ok := p.index[name]; ok {
			return i
		}
	}
	return -1
}

func newContentProjection(ctx context.Context, db *sql.DB, table string) (contentProjection, error) {
	p, err := buildSQLProjection(ctx, db, table, contentColumnGroups)
	if err != nil {
		return contentProjection{}, err
	}
	return contentProjection{
		sql:          p,
		id:           contentField(p, "id", "content_id", "track_id"),
		path:         contentField(p, "file_path", "path", "location"),
		fileName:     contentField(p, "file_name", "file_name_l", "filename"),
		folderPath:   contentField(p, "folder_path"),
		bpmX100:      contentField(p, "bpmx100"),
		bpm:          contentField(p, "bpm"),
		keyName:      contentField(p, "key_name"),
		keyID:        contentField(p, "key_id"),
		key:          contentField(p, "key"),
		imageID:      contentField(p, "image_id"),
		artworkID:    contentField(p, "artwork_id"),
		imagePath:    contentField(p, "image_path"),
		fileSize:     contentField(p, "file_size"),
		analysisPath: contentField(p, "analysisDataFilePath", "analysis_data_path", "analyze_path", "analysis_path"),
		title:        contentField(p, "title", "name"),
		artistID:     contentField(p, "artist_id_artist", "artist_id"),
		composerID:   contentField(p, "artist_id_composer", "composer_id"),
		remixerID:    contentField(p, "artist_id_remixer", "remixer_id"),
		albumID:      contentField(p, "album_id"),
		album:        contentField(p, "album"),
		genreID:      contentField(p, "genre_id"),
		labelID:      contentField(p, "label_id"),
		length:       contentField(p, "length", "duration"),
		sampleRate:   contentField(p, "samplingRate", "sample_rate"),
		bitDepth:     contentField(p, "bit_depth"),
		bitRate:      contentField(p, "bit_rate", "bitrate"),
		releaseYear:  contentField(p, "release_year", "year"),
		rating:       contentField(p, "rating"),
		comment:      contentField(p, "djComment", "commnt", "comment"),
		artist:       contentField(p, "artist"),
	}, nil
}

func driverValueAt(values []driver.Value, index int) driver.Value {
	if index < 0 || index >= len(values) {
		return nil
	}
	return values[index]
}

func driverStringAt(values []driver.Value, index int) string {
	return driverString(driverValueAt(values, index))
}

func driverIntAt(values []driver.Value, index int) (int64, bool) {
	return driverInt(driverValueAt(values, index))
}

func driverNumAt(values []driver.Value, index int) uint32 {
	n, ok := driverIntAt(values, index)
	if !ok || n < 0 || n > math.MaxUint32 {
		return 0
	}
	return uint32(n)
}

func driverDecimalAt(values []driver.Value, index int) float64 {
	return driverDecimal(driverValueAt(values, index))
}

func driverDecimal(v driver.Value) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case uint64:
		return float64(x)
	case float64:
		return x
	case []byte:
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	default:
		return 0
	}
}

func parseContentBPM(values []driver.Value, p contentProjection) float64 {
	if raw := driverStringAt(values, p.bpmX100); raw != "" {
		if iv, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if iv <= 0 || iv == 0x7fffffff || iv == 0xffffffff {
				return 0
			}
			return float64(iv) / 100
		}
		fv := driverDecimalAt(values, p.bpmX100)
		if fv <= 0 || fv == 0x7fffffff || fv == 0xffffffff {
			return 0
		}
		return fv / 100
	}
	if iv, ok := driverIntAt(values, p.bpm); ok && (iv <= 0 || iv == 0x7fffffff || iv == 0xffffffff) {
		return 0
	}
	bpm := driverDecimalAt(values, p.bpm)
	if bpm > 1000 {
		bpm /= 100
	}
	return bpm
}

func eachSQLContentRow(ctx context.Context, db *sql.DB, table string, fn func([]driver.Value, contentProjection)) error {
	p, err := newContentProjection(ctx, db, table)
	if err != nil {
		return err
	}
	return p.sql.each(ctx, db, func(values []driver.Value) { fn(values, p) })
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
			if e := eachSQLProjectedRow(ctx, db, tbl, lookupColumnGroups(kind), func(r sqlDriverRecord) {
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
	if e := eachSQLContentRow(ctx, db, contentTable, func(values []driver.Value, p contentProjection) {
		path := driverStringAt(values, p.path)
		if filename := driverStringAt(values, p.fileName); path == "" && filename != "" {
			if folder := driverStringAt(values, p.folderPath); folder != "" {
				if strings.HasSuffix(folder, "/"+filename) || strings.HasSuffix(folder, `\`+filename) {
					path = folder
				} else {
					path = strings.TrimRight(folder, "/") + "/" + filename
				}
			}
		}
		bpm := parseContentBPM(values, p)
		key := driverStringAt(values, p.keyName)
		if key == "" {
			key = lookups["key"][driverNumAt(values, p.keyID)]
			if key == "" {
				key = driverStringAt(values, p.key)
			}
		}
		artworkPath := lookups["image"][driverNumAt(values, p.imageID)]
		if artworkPath == "" {
			artworkPath = l.Artwork[driverNumAt(values, p.artworkID)]
		}
		if artworkPath == "" {
			artworkPath = driverStringAt(values, p.imagePath)
		}
		var fileSize int64
		if sz, ok := driverIntAt(values, p.fileSize); ok && sz > 0 {
			fileSize = sz
		} else if fsz := driverDecimalAt(values, p.fileSize); fsz > 0 && fsz < 0x1p63 {
			fileSize = int64(fsz)
		}
		// OneLibrary content.length is stored in whole seconds (verified against
		// libdjinterop content_table.cpp std::chrono::seconds{*length}).
		t := Track{ID: driverNumAt(values, p.id), Path: path, AnalysisPath: driverStringAt(values, p.analysisPath), Title: driverStringAt(values, p.title), Artist: lookups["artist"][driverNumAt(values, p.artistID)], Composer: lookups["artist"][driverNumAt(values, p.composerID)], Remixer: lookups["artist"][driverNumAt(values, p.remixerID)], Album: lookups["album"][driverNumAt(values, p.albumID)], Genre: lookups["genre"][driverNumAt(values, p.genreID)], Label: lookups["label"][driverNumAt(values, p.labelID)], Key: key, BPM: bpm, Duration: driverDecimalAt(values, p.length), SampleRate: int(driverNumAt(values, p.sampleRate)), BitDepth: int(driverNumAt(values, p.bitDepth)), Bitrate: int(driverNumAt(values, p.bitRate)), FileSize: fileSize, Year: int(driverNumAt(values, p.releaseYear)), Rating: int(driverNumAt(values, p.rating)), Comment: driverStringAt(values, p.comment), ArtworkPath: artworkPath}
		if t.Artist == "" {
			t.Artist = driverStringAt(values, p.artist)
		}
		if t.Album == "" {
			t.Album = driverStringAt(values, p.album)
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
		if e = eachSQLProjectedRow(ctx, db, cueTable, cueColumnGroups, func(r sqlDriverRecord) {
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
		if e := eachSQLProjectedRow(ctx, db, plTable, playlistColumnGroups, func(r sqlDriverRecord) {
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
		if e := eachSQLProjectedRow(ctx, db, pcTable, playlistContentColumnGroups, func(r sqlDriverRecord) {
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

// sqlitePagesWhole reports an error when the SQLite file at path is not a whole number of its
// pages, or not the number of pages its header gives where that count is valid (nonzero, written
// with the change counter it is valid for).
func sqlitePagesWhole(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := make([]byte, 100)
	if _, err = f.ReadAt(h, 0); err != nil || string(h[:16]) != "SQLite format 3\x00" {
		return fmt.Errorf("OneLibrary SQLite: no SQLite header")
	}
	ps := int64(binary.BigEndian.Uint16(h[16:]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || st.Size()%ps != 0 {
		return fmt.Errorf("OneLibrary SQLite: %d bytes are not whole pages of %d", st.Size(), ps)
	}
	if n := int64(binary.BigEndian.Uint32(h[28:])); n != 0 && bytes.Equal(h[24:28], h[92:96]) && n*ps != st.Size() {
		return fmt.Errorf("OneLibrary SQLite: %d bytes, the header says %d pages of %d", st.Size(), n, ps)
	}
	return nil
}
