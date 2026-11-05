package analyzer

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const DefaultDatabase = "/media/sc5000-sd-cache/Engine Library/Database2/m.db"

// Library opens an existing Engine 5.0.4 SD library. It never replaces a live
// database or invents a new schema/identity. SQLite transactions cooperate with
// Engine's connections; a lock timeout leaves the entire import uncommitted.
type Library struct {
	db         *sql.DB
	root, uuid string
}

func OpenLibrary(path string) (*Library, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("library: create the SD library in Engine first: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	if filepath.Base(abs) != "m.db" || filepath.Base(filepath.Dir(abs)) != "Database2" || filepath.Base(filepath.Dir(filepath.Dir(abs))) != "Engine Library" {
		return nil, errors.New("library: expected <volume>/Engine Library/Database2/m.db")
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=rw&_pragma=busy_timeout(1500)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)&_txlock=immediate"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	l := &Library{db: db, root: filepath.Dir(filepath.Dir(filepath.Dir(abs)))}
	var major, minor, patch int
	err = db.QueryRow("SELECT uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information").Scan(&l.uuid, &major, &minor, &patch)
	if err != nil || l.uuid == "" || major != 3 || minor != 0 || patch != 2 {
		db.Close()
		return nil, errors.New("library: expected an Engine 5.0.4 Database2 schema 3.0.2 with UUID")
	}
	return l, nil
}
func (l *Library) Close() error { return l.db.Close() }

// Root is the volume holding this Engine Library.
func (l *Library) Root() string { return l.root }

type Import struct {
	Path, Title, Artist, Album, Comment, Playlist, Format string
	Result                                                *Result
}

// Import atomically links metadata, verified performance blobs and playlist
// membership. quickCues/loops are deliberately omitted, including on reimport.
func (l *Library) Import(ctx context.Context, in Import) (int64, error) {
	r := in.Result
	if r == nil || r.SampleRate < 8000 || r.SampleRate > 384000 || r.TotalSamples <= 0 ||
		r.Duration <= 0 || math.IsNaN(r.Duration) || math.IsInf(r.Duration, 0) ||
		r.BPM <= 0 || math.IsNaN(r.BPM) || math.IsInf(r.BPM, 0) ||
		r.OverviewSamplesPerEntry <= 0 || math.IsNaN(r.OverviewSamplesPerEntry) || math.IsInf(r.OverviewSamplesPerEntry, 0) ||
		len(r.Overview) != OverviewEntries || len(r.Beatgrid) < 2 {
		return 0, errors.New("library: incomplete analysis")
	}
	if in.Playlist == "" {
		return 0, errors.New("library: playlist is required")
	}
	p, err := filepath.EvalSymlinks(in.Path)
	if err != nil {
		return 0, err
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return 0, err
	}
	root, err := filepath.EvalSymlinks(l.root)
	if err != nil {
		return 0, err
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return 0, errors.New("library: audio must reside on the same SD volume")
	}
	// Engine resolves Track.path from Engine Library, not the volume root.
	engineRel, err := filepath.Rel(filepath.Join(root, "Engine Library"), p)
	if err != nil {
		return 0, err
	}
	enginePath := filepath.ToSlash(engineRel)
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	if !st.Mode().IsRegular() {
		return 0, errors.New("library: audio is not a regular file")
	}
	track, overview, beats, err := PerformanceBlobs(r)
	if err != nil {
		return 0, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	var alreadyAnalyzed, beatGridLocked bool
	format := in.Format
	if format == "" {
		format = strings.TrimPrefix(strings.ToLower(filepath.Ext(p)), ".")
	}
	kbps := int(math.Round(float64(st.Size()) * 8 / r.Duration / 1000)) // Engine stores kbit/s (#34)
	err = tx.QueryRowContext(ctx, "SELECT id FROM Track WHERE path=?", enginePath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		result, e := tx.ExecContext(ctx, `INSERT INTO Track(path,filename,title,artist,album,comment,length,bpm,bpmAnalyzed,key,fileBytes,bitrate,fileType,isAnalyzed,isAvailable,isMetadataImported,isBeatGridLocked,dateCreated,dateAdded,lastEditTime)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,1,1,1,0,strftime('%s'),strftime('%s'),strftime('%s'))`, enginePath, filepath.Base(p), in.Title, in.Artist, in.Album, in.Comment, int(math.Round(r.Duration)), int(math.Round(r.BPM)), r.BPM, r.Key.EngineID, st.Size(), kbps, format)
		if e != nil {
			return 0, e
		}
		id, err = result.LastInsertId()
	} else if err == nil {
		err = tx.QueryRowContext(ctx, "SELECT coalesce(isAnalyzed,0),coalesce(isBeatGridLocked,0) FROM Track WHERE id=?", id).Scan(&alreadyAnalyzed, &beatGridLocked)
		if err != nil {
			return 0, err
		}
		if alreadyAnalyzed {
			// Keep the analysis and the owner's tags, but describe the file now on disk (#34).
			_, err = tx.ExecContext(ctx, "UPDATE Track SET comment=?,fileBytes=?,bitrate=?,fileType=?,length=?,isAvailable=1 WHERE id=?", in.Comment, st.Size(), kbps, format, int(math.Round(r.Duration)), id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE Track SET title=?,artist=?,album=?,filename=?,fileBytes=?,bitrate=?,fileType=?,comment=?,bpm=CASE WHEN isBeatGridLocked AND bpm>0 THEN bpm ELSE ? END,bpmAnalyzed=CASE WHEN isBeatGridLocked AND bpmAnalyzed>0 THEN bpmAnalyzed ELSE ? END,key=?,length=?,isAnalyzed=1,isAvailable=1 WHERE id=?`, in.Title, in.Artist, in.Album, filepath.Base(p), st.Size(), kbps, format, in.Comment, int(math.Round(r.BPM)), r.BPM, r.Key.EngineID, int(math.Round(r.Duration)), id)
		}
	}
	if err != nil {
		return 0, err
	}
	// Preserve an existing analysis; a locked grid may coexist with missing waveform data.
	if alreadyAnalyzed {
		// ...but fill in waveforms an analysed track lacks (#34).
		_, err = tx.ExecContext(ctx, `UPDATE PerformanceData SET trackData=coalesce(trackData,?),overviewWaveFormData=coalesce(overviewWaveFormData,?) WHERE trackId=? AND (trackData IS NULL OR overviewWaveFormData IS NULL)`, track, overview, id)
		if err != nil {
			return 0, err
		}
	} else {
		// The original trigger creates PerformanceData on Track insertion.
		_, err = tx.ExecContext(ctx, `INSERT INTO PerformanceData(trackId,trackData,overviewWaveFormData,beatData) VALUES(?,?,?,?) ON CONFLICT(trackId) DO UPDATE SET trackData=excluded.trackData,overviewWaveFormData=excluded.overviewWaveFormData,beatData=CASE WHEN ? AND PerformanceData.beatData IS NOT NULL THEN PerformanceData.beatData ELSE excluded.beatData END`, id, track, overview, beats, beatGridLocked)
		if err != nil {
			return 0, err
		}
	}
	var listID int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM Playlist WHERE title=? AND parentListId=0", in.Playlist).Scan(&listID)
	if errors.Is(err, sql.ErrNoRows) {
		// Original BEFORE/AFTER INSERT triggers splice the new list at the tail.
		rr, e := tx.ExecContext(ctx, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported,lastEditTime) VALUES(?,0,1,0,1,strftime('%s'))`, in.Playlist)
		if e != nil {
			return 0, e
		}
		listID, err = rr.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	var exists int
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM PlaylistEntity WHERE listId=? AND trackId=? AND databaseUuid=?", listID, id, l.uuid).Scan(&exists)
	if err != nil {
		return 0, err
	}
	if exists == 0 {
		rr, e := tx.ExecContext(ctx, "INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,?,?,0,0)", listID, id, l.uuid)
		if e != nil {
			return 0, e
		}
		entityID, e := rr.LastInsertId()
		if e != nil {
			return 0, e
		}
		// A list is a singly linked chain ending at nextEntityId=0 (or NULL in some
		// databases). Link exactly one tail, the newest, even in a damaged list (#34).
		if _, err = tx.ExecContext(ctx, "UPDATE PlaylistEntity SET nextEntityId=? WHERE id=(SELECT max(id) FROM PlaylistEntity WHERE listId=? AND coalesce(nextEntityId,0)=0 AND id<>?)", entityID, listID, entityID); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func qCompress(raw []byte) ([]byte, error) {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(raw)))
	z := zlib.NewWriter(&b)
	if _, err := z.Write(raw); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// PerformanceBlobs follows the observed 5.0.4 layouts, not the obsolete p.db
// format. Reserved/unknown fields are zero. The trackData enum and RGB channel
// interpretation remain inferences; see docs/stream-sync.md for device gates.
func PerformanceBlobs(r *Result) (track, overview, beats []byte, err error) {
	var t, o, b bytes.Buffer
	binary.Write(&t, binary.BigEndian, float64(r.SampleRate))
	binary.Write(&t, binary.BigEndian, uint64(r.TotalSamples))
	binary.Write(&t, binary.BigEndian, uint32(r.Key.EngineID))
	for i := 0; i < 3; i++ {
		binary.Write(&t, binary.BigEndian, float64(0))
	}
	binary.Write(&o, binary.BigEndian, uint64(OverviewEntries))
	binary.Write(&o, binary.BigEndian, uint64(len(r.Overview)))
	binary.Write(&o, binary.BigEndian, r.OverviewSamplesPerEntry)
	for _, p := range r.Overview {
		o.Write([]byte{p.LowValue, p.MidValue, p.HighValue})
	}
	o.Write([]byte{0, 0, 0})
	binary.Write(&b, binary.BigEndian, float64(r.SampleRate))
	binary.Write(&b, binary.BigEndian, float64(r.TotalSamples))
	b.WriteByte(1)
	for grid := 0; grid < 2; grid++ {
		binary.Write(&b, binary.BigEndian, uint64(len(r.Beatgrid)))
		for _, m := range r.Beatgrid {
			binary.Write(&b, binary.LittleEndian, m.SampleOffset)
			binary.Write(&b, binary.LittleEndian, m.BeatNumber)
			binary.Write(&b, binary.LittleEndian, m.NumBeats)
			binary.Write(&b, binary.LittleEndian, uint32(0))
		}
	}
	b.WriteByte(0)
	if track, err = qCompress(t.Bytes()); err != nil {
		return
	}
	if overview, err = qCompress(o.Bytes()); err != nil {
		return
	}
	beats, err = qCompress(b.Bytes())
	return
}
