package prolink

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	_ "modernc.org/sqlite"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Serialize callers so a second start cannot overwrite a batch's checkpoint.
var engineExportMu sync.Mutex
var engineExportWorkers sync.Map // absolute root -> *engineExportWorker

type engineExportWorker struct {
	stop chan struct{}
	done chan struct{}
}

type engineExportJob struct {
	engineID, exportID uint32
	path               string
	length             int
	artID              int64
}

const engineExportBatch = 32

type engineExportProgress struct {
	Stamp  string
	LastID uint32
	IDs    map[uint32]uint32
}

// SyncEngineToPioneerExport writes root's export.pdb so that Pioneer CDJ players
// can browse and play over Pro DJ Link (dbserver + NFS): the Engine library in
// <root>/Engine Library/Database2/m.db, if any, and the EngineX OS streaming
// downloads (StreamDirs) as /EngineX_Stream/<name> in an "EngineX Streaming"
// folder. A real rekordbox export is left alone; an unchanged source is a no-op.
func SyncEngineToPioneerExport(root string) error {
	engineExportMu.Lock()
	defer engineExportMu.Unlock()
	var err error
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	if active, ok := engineExportWorkers.Load(root); ok {
		worker := active.(*engineExportWorker)
		close(worker.stop)
		<-worker.done
	}
	started := time.Now()
	mdbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	fi, err := os.Stat(mdbPath)
	hasMDB := err == nil && fi.Mode().IsRegular()
	streams := scanStreams()
	if !hasMDB && len(streams) == 0 {
		return nil
	}

	pdbPath := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
	stampPath := filepath.Join(root, "PIONEER", "rekordbox", ".enginex-stamp")
	sourceStamp := engineSourceStamp(mdbPath)
	currentStamp := "analysis-v3|" + sourceStamp + "|" + streamStamp(streams)
	progressPath := filepath.Join(root, "PIONEER", "rekordbox", ".enginex-progress")
	progress := engineExportProgress{IDs: make(map[uint32]uint32)}
	if b, err := os.ReadFile(progressPath); err == nil {
		var saved engineExportProgress
		if json.Unmarshal(b, &saved) == nil {
			progress = saved
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "PIONEER/rekordbox/.enginex-cursor")); err == nil {
		var cursor engineExportProgress
		if json.Unmarshal(b, &cursor) == nil && cursor.Stamp == progress.Stamp && cursor.LastID > progress.LastID {
			progress.LastID = cursor.LastID
		}
	}
	if progress.IDs == nil {
		progress.IDs = make(map[uint32]uint32)
	}
	if progress.Stamp != currentStamp {
		progress.LastID = 0
	}
	progress.Stamp = currentStamp
	var jobs []engineExportJob
	processed := 0
	artResolver, _ := compat.NewResolver(root)

	// A real rekordbox export (including Rekordbox 7 exportLibrary.db or macOS .PIONEER)
	// is the DJ's own: only an export this bridge wrote is rebuilt.
	if hasNativeRekordboxExport(root) {
		return nil
	}

	var existingLib *rekordbox.Library
	if pdbFi, err := os.Stat(pdbPath); err == nil && pdbFi.Mode().IsRegular() {
		if stampBytes, err := os.ReadFile(stampPath); err == nil && string(stampBytes) == currentStamp {
			return nil
		}
		if el, err := rekordbox.ReadPDB(pdbPath); err == nil && el != nil {
			existingLib = el
			dropStaleStream(existingLib, streams)
		}
	}

	existingPaths := make(map[string]rekordbox.Track)
	maxTrackID := uint32(0)
	if existingLib != nil {
		for _, t := range existingLib.Tracks {
			if t.ID > maxTrackID {
				maxTrackID = t.ID
			}
			norm := "/" + strings.TrimPrefix(filepath.ToSlash(filepath.Clean(t.Path)), "/")
			existingPaths[norm] = t
		}
	}

	existingPlaylistNames := make(map[string]rekordbox.Playlist)
	usedPlaylistIDs := make(map[uint32]bool)
	maxPlaylistID := uint32(0)
	if existingLib != nil {
		for _, p := range existingLib.Playlists {
			if p.ID > maxPlaylistID {
				maxPlaylistID = p.ID
			}
			usedPlaylistIDs[p.ID] = true
			existingPlaylistNames[fmt.Sprintf("%d/%s", p.ParentID, p.Name)] = p
		}
	}

	trackIDMap := progress.IDs
	trackMap := make(map[uint32]bool)
	if existingLib != nil {
		for _, t := range existingLib.Tracks {
			trackMap[t.ID] = true
		}
	}
	var newTracks []rekordbox.Track
	var newPlaylists []rekordbox.Playlist

	if hasMDB {
		db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdbPath, RawQuery: "mode=ro&_pragma=busy_timeout(500)"}).String()) // read-only: never create -wal/-journal on the user's USB drive
		if err != nil {
			return fmt.Errorf("open engine m.db: %w", err)
		}
		defer db.Close()

		// Older hand-built/test databases may not contain the optional analysis columns.
		keyColumn, artColumn, bytesColumn, beatHeaderColumn := "NULL", "0", "0", "NULL"
		if engineColumn(db, "Track", "key") {
			keyColumn = "key"
		}
		if engineColumn(db, "Track", "albumArtId") {
			artColumn = "COALESCE(albumArtId, 0)"
		}
		if engineColumn(db, "Track", "fileBytes") {
			bytesColumn = "COALESCE(fileBytes, 0)"
		}
		if engineColumn(db, "PerformanceData", "beatData") {
			beatHeaderColumn = "substr((SELECT beatData FROM PerformanceData WHERE trackId=Track.id),1,2097153)"
		}
		trackRows, err := db.Query(`SELECT id, COALESCE(title, ''), COALESCE(artist, ''), COALESCE(bpm, 0), COALESCE(bpmAnalyzed, 0), COALESCE(length, 0), COALESCE(path, ''), COALESCE(filename, ''), ` + keyColumn + `, ` + artColumn + `, ` + bytesColumn + `, ` + beatHeaderColumn + ` FROM Track ORDER BY id`)
		if err != nil {
			return fmt.Errorf("query engine tracks: %w", err)
		}
		defer trackRows.Close()

		seenEngineIDs := make(map[uint32]bool)
		for trackRows.Next() {
			var (
				id          uint32
				title       string
				artist      string
				bpmInt      int
				bpmAnalyzed float64
				length      int
				rawPath     string
				filename    string
				key         sql.NullInt64
				artID       int64
				fileSize    int64
				beatHeader  []byte
			)
			if err := trackRows.Scan(&id, &title, &artist, &bpmInt, &bpmAnalyzed, &length, &rawPath, &filename, &key, &artID, &fileSize, &beatHeader); err != nil {
				return fmt.Errorf("scan engine track: %w", err)
			}
			if id == 0 {
				continue
			}
			seenEngineIDs[id] = true
			if title == "" {
				if filename != "" {
					title = strings.TrimSuffix(filename, filepath.Ext(filename))
				} else {
					title = fmt.Sprintf("Track %d", id)
				}
			}
			effectiveBPM := bpmAnalyzed
			if effectiveBPM <= 0 && bpmInt > 0 {
				effectiveBPM = float64(bpmInt)
			}
			if !finiteEngine(effectiveBPM) || effectiveBPM < 0 || effectiveBPM > 1000 {
				effectiveBPM = 0
			}
			if length < 0 {
				length = 0
			}

			cleanPath := rawPath
			for strings.HasPrefix(cleanPath, "../") {
				cleanPath = strings.TrimPrefix(cleanPath, "../")
			}
			for strings.HasPrefix(cleanPath, "./") {
				cleanPath = strings.TrimPrefix(cleanPath, "./")
			}
			cleanPath = filepath.Clean(cleanPath)
			cleanPath = "/" + strings.TrimPrefix(filepath.ToSlash(cleanPath), "/")

			newID := id
			if mapped := trackIDMap[id]; mapped != 0 {
				newID = mapped
			} else if exist, ok := existingPaths[cleanPath]; ok {
				newID = exist.ID
			} else if trackMap[newID] {
				maxTrackID++
				newID = maxTrackID
			}
			if newID > maxTrackID {
				maxTrackID = newID
			}
			trackIDMap[id] = newID
			trackMap[newID] = true

			analysisPath := engineAnalysisPath(newID)
			var analysis *engineExportAnalysis
			job := engineExportJob{id, newID, cleanPath, length, artID}
			if id > progress.LastID {
				if len(jobs) == 0 && processed < engineExportBatch && (processed == 0 || time.Since(started) < 750*time.Millisecond) {
					analysis, err = readEngineAnalysis(db, root, id, cleanPath, length)
					if err != nil {
						return fmt.Errorf("engine analysis %d: %w", id, err)
					}
					if _, err := writeANLZ(root, newID, cleanPath, analysis.Analysis); err != nil {
						return err
					}
					processed++
					progress.LastID = id
				} else {
					jobs = append(jobs, job)
				}
			}
			if fileSize <= 0 {
				if st, err := os.Stat(filepath.Join(root, cleanPath)); err == nil && st.Mode().IsRegular() {
					fileSize = st.Size() // Older databases may omit fileBytes.
				}
			}

			t := rekordbox.Track{
				ID:           newID,
				Path:         cleanPath,
				AnalysisPath: analysisPath,
				Title:        title,
				Artist:       artist,
				BPM:          effectiveBPM,
				Duration:     float64(length),
				SampleRate:   0,
				BitDepth:     24,
				Bitrate:      320,
				Year:         2026,
				FileSize:     fileSize,
				Key:          engineExportKey(key),
			}
			if analysis != nil {
				t.SampleRate = int(analysis.rate)
				if t.Duration <= 0 {
					t.Duration = analysis.duration
				}
			} else {
				rate, duration := engineAudioMetadata(beatHeader)
				t.SampleRate = int(rate)
				if t.Duration <= 0 {
					t.Duration = duration
				}
			}
			if artID > 0 {
				t.ArtworkPath = engineArtworkPath(artID)
				if analysis != nil {
					t.ArtworkPath, err = exportEngineArtwork(db, root, artID, artResolver)
					if err != nil {
						log.Printf("prolink: Engine track %d: omit unavailable artwork: %v", id, err)
					}
				}
			}
			newTracks = append(newTracks, t)
		}
		if err := trackRows.Err(); err != nil {
			return fmt.Errorf("read engine tracks: %w", err)
		}
		trackRows.Close()
		for id := range trackIDMap {
			if !seenEngineIDs[id] {
				delete(trackIDMap, id)
			}
		}

		playlistRows, err := db.Query(`SELECT id, COALESCE(title, ''), COALESCE(parentListId, 0) FROM Playlist WHERE title NOT LIKE '[Pro DJ Link:%' ORDER BY id`)
		if err != nil {
			return fmt.Errorf("query engine playlists: %w", err)
		}
		defer playlistRows.Close()

		type rawPlaylist struct {
			id     uint32
			name   string
			parent uint32
		}
		var rawPlaylists []rawPlaylist
		parentCounts := make(map[uint32]int)

		for playlistRows.Next() {
			var id, parent uint32
			var name string
			if err := playlistRows.Scan(&id, &name, &parent); err != nil {
				return fmt.Errorf("scan playlist: %w", err)
			}
			if id == 0 {
				continue
			}
			rawPlaylists = append(rawPlaylists, rawPlaylist{id: id, name: name, parent: parent})
			if parent != 0 {
				parentCounts[parent]++
			}
		}

		entitiesByList := make(map[uint32][]uint32)
		entityRows, err := db.Query(`SELECT id, listId, trackId, COALESCE(nextEntityId, 0) FROM PlaylistEntity ORDER BY id`)
		if err == nil {
			listEnts := make(map[uint32][]enginePlaylistEntity)
			for entityRows.Next() {
				var id, listID, trackID, nextID uint32
				if err := entityRows.Scan(&id, &listID, &trackID, &nextID); err != nil {
					entityRows.Close()
					return err
				}
				// Keep missing tracks in the chain: their successors still have an order.
				listEnts[listID] = append(listEnts[listID], enginePlaylistEntity{id, trackIDMap[trackID], nextID})
			}
			if err := entityRows.Err(); err != nil {
				entityRows.Close()
				return err
			}
			entityRows.Close()

			for listId, ents := range listEnts {
				entitiesByList[listId] = enginePlaylistOrder(ents)
			}
		}

		playlistIDMap := make(map[uint32]uint32)

		for _, rp := range rawPlaylists {
			name := rp.name
			if name == "" {
				name = fmt.Sprintf("Playlist %d", rp.id)
			}
			parentID := uint32(0)
			if rp.parent != 0 {
				parentID = playlistIDMap[rp.parent]
			}
			k := fmt.Sprintf("%d/%s", parentID, name)
			if existing, ok := existingPlaylistNames[k]; ok {
				playlistIDMap[rp.id] = existing.ID
			}

			newPID := rp.id
			if mapped := playlistIDMap[rp.id]; mapped != 0 {
				newPID = mapped
			} else if newPID == 0 || usedPlaylistIDs[newPID] {
				maxPlaylistID++
				newPID = maxPlaylistID
			}
			if newPID > maxPlaylistID {
				maxPlaylistID = newPID
			}
			usedPlaylistIDs[newPID] = true
			playlistIDMap[rp.id] = newPID

			isFolder := parentCounts[rp.id] > 0
			var memberTracks []uint32
			if !isFolder {
				memberTracks = entitiesByList[rp.id]
			}

			pl := rekordbox.Playlist{
				ID:       newPID,
				ParentID: parentID,
				Name:     name,
				Folder:   isFolder,
				Tracks:   memberTracks,
			}
			newPlaylists = append(newPlaylists, pl)
			existingPlaylistNames[k] = pl
		}
	}

	if len(streams) > 0 {
		meta := internalMeta()
		byPlatform := map[string][]uint32{}
		var all []uint32
		for _, st := range streams {
			path := "/" + StreamPrefix + "/" + st.name
			id := existingPaths[path].ID
			if id == 0 {
				m := streamMeta(st, meta[strings.ToLower(st.name)])
				maxTrackID++
				id = maxTrackID
				analysisPath := engineAnalysisPath(id)
				if processed < engineExportBatch && (processed == 0 || time.Since(started) < 750*time.Millisecond) {
					if _, err := writeANLZ(root, id, path, &rekordbox.Analysis{}); err != nil {
						return err
					}
					processed++
				} else {
					jobs = append(jobs, engineExportJob{exportID: id, path: path})
				}
				m.ID, m.Path, m.AnalysisPath = id, path, analysisPath
				newTracks = append(newTracks, m)
			}
			if existingPaths[path].ID != 0 {
				if _, err := os.Stat(filepath.Join(root, engineAnalysisPath(id))); os.IsNotExist(err) {
					jobs = append(jobs, engineExportJob{exportID: id, path: path})
				}
			}
			all = append(all, id)
			byPlatform[st.platform] = append(byPlatform[st.platform], id)
		}
		streamPlaylistID := func(parent uint32, name string) uint32 {
			if old, ok := existingPlaylistNames[fmt.Sprintf("%d/%s", parent, name)]; ok {
				return old.ID
			}
			maxPlaylistID++
			return maxPlaylistID
		}
		folder := streamPlaylistID(0, StreamFolder)
		allID := streamPlaylistID(folder, "All Streaming")
		newPlaylists = append(newPlaylists, rekordbox.Playlist{ID: folder, Name: StreamFolder, Folder: true}, rekordbox.Playlist{ID: allID, ParentID: folder, Name: "All Streaming", Tracks: all})
		platforms := make([]string, 0, len(byPlatform))
		for p := range byPlatform {
			platforms = append(platforms, p)
		}
		sort.Strings(platforms)
		for _, p := range platforms {
			id := streamPlaylistID(folder, p)
			newPlaylists = append(newPlaylists, rekordbox.Playlist{ID: id, ParentID: folder, Name: p, Tracks: byPlatform[p]})
		}
	}

	if existingLib != nil && len(newTracks) == 0 && len(newPlaylists) == 0 && len(jobs) == 0 {
		_ = writeEngineExportFile(stampPath, []byte(currentStamp), 0644)
		return nil
	}

	var mergedTracks []rekordbox.Track
	if existingLib != nil {
		for _, t := range existingLib.Tracks {
			if !hasMDB || strings.HasPrefix(t.Path, "/"+StreamPrefix+"/") {
				mergedTracks = append(mergedTracks, t)
			}
		}
	}
	trackIndex := make(map[uint32]int, len(mergedTracks))
	for i, t := range mergedTracks {
		trackIndex[t.ID] = i
	}
	for _, t := range newTracks {
		if i, ok := trackIndex[t.ID]; ok {
			mergedTracks[i] = t
		} else {
			trackIndex[t.ID] = len(mergedTracks)
			mergedTracks = append(mergedTracks, t)
		}
	}

	var mergedPlaylists []rekordbox.Playlist
	if existingLib != nil && !hasMDB {
		mergedPlaylists = append(mergedPlaylists, existingLib.Playlists...)
	}
	playlistIndex := make(map[uint32]int, len(mergedPlaylists))
	for i, p := range mergedPlaylists {
		playlistIndex[p.ID] = i
	}
	for _, p := range newPlaylists {
		if i, ok := playlistIndex[p.ID]; ok {
			mergedPlaylists[i] = p
		} else {
			playlistIndex[p.ID] = len(mergedPlaylists)
			mergedPlaylists = append(mergedPlaylists, p)
		}
	}

	for i := range mergedPlaylists {
		mergedPlaylists[i].SortOrder = uint32(i + 1)
	}

	pdbData, err := buildPDB(mergedTracks, mergedPlaylists)
	if err != nil {
		return fmt.Errorf("build export.pdb: %w", err)
	}

	if _, err := compat.SafeOutputDir(root, "PIONEER/rekordbox"); err != nil {
		return fmt.Errorf("mkdir pioneer dir: %w", err)
	}

	if err := writeEngineExportFile(pdbPath, pdbData, 0644); err != nil {
		return fmt.Errorf("write export.pdb: %w", err)
	}
	if st, err := os.Stat(pdbPath); err != nil || writeEngineExportFile(filepath.Join(root, generatedMarker), []byte(pdbStamp(st)), 0644) != nil {
		return fmt.Errorf("mark generated export.pdb: %v", err)
	}

	lib, err := rekordbox.ReadPDB(pdbPath)
	if err != nil {
		return fmt.Errorf("validate generated export.pdb: %w", err)
	}
	if err := lib.Validate(); err != nil {
		return fmt.Errorf("validate library: %w", err)
	}

	progressBytes, err := json.Marshal(progress)
	if err != nil {
		return err
	}
	if err := writeEngineExportFile(progressPath, progressBytes, 0644); err != nil {
		return err
	}
	if len(jobs) == 0 {
		if engineSourceStamp(mdbPath) == sourceStamp {
			return writeEngineExportFile(stampPath, []byte(currentStamp), 0644)
		}
	} else {
		startEngineExportWorker(root, mdbPath, sourceStamp, currentStamp, progress, jobs)
	}
	return nil
}

// Full browse metadata is published once. Expensive analysis and cover files
// finish in a single worker per root, so startExport need not reload its immutable
// Library and a large USB never needs repeated OFF/ON cycles to become visible.
func engineAnalysisPath(id uint32) string {
	return fmt.Sprintf("/PIONEER/USBANLZ/P%03d/ANLZ0000.DAT", id)
}

func engineArtworkPath(id int64) string {
	return fmt.Sprintf("/PIONEER/Artwork/A%d.jpg", id)
}

func engineSourceStamp(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	stamp := pdbStamp(st)
	if wal, err := os.Stat(path + "-wal"); err == nil {
		stamp += "|wal:" + pdbStamp(wal)
	}
	return stamp
}

func startEngineExportWorker(root, mdbPath, sourceStamp, stamp string, progress engineExportProgress, jobs []engineExportJob) {
	marker, err := os.ReadFile(filepath.Join(root, generatedMarker))
	if err != nil {
		return
	}
	worker := &engineExportWorker{stop: make(chan struct{}), done: make(chan struct{})}
	engineExportWorkers.Store(root, worker)
	go func() {
		defer func() { engineExportWorkers.CompareAndDelete(root, worker); close(worker.done) }()
		current := func() bool {
			select {
			case <-worker.stop:
				return false
			default:
			}
			b, err := os.ReadFile(filepath.Join(root, generatedMarker))
			if err != nil || !bytes.Equal(b, marker) {
				return false
			}
			st, err := os.Stat(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
			return err == nil && pdbStamp(st) == string(marker)
		}
		var db *sql.DB
		for _, job := range jobs {
			if job.engineID != 0 {
				db, err = sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdbPath, RawQuery: "mode=ro&_pragma=busy_timeout(500)"}).String())
				if err != nil {
					log.Printf("prolink: open background Engine export: %v", err)
					return
				}
				defer db.Close()
				break
			}
		}
		resolver, _ := compat.NewResolver(root)
		checkpoint := func() error {
			// IDs were persisted with the metadata; only the tiny cursor changes here.
			b, err := json.Marshal(engineExportProgress{Stamp: stamp, LastID: progress.LastID})
			if err != nil {
				return err
			}
			return writeEngineExportFile(filepath.Join(root, "PIONEER/rekordbox/.enginex-cursor"), b, 0644)
		}
		batchStart := time.Now()
		batch := 0
		artDone := make(map[int64]bool)
		for _, job := range jobs {
			if !current() {
				return
			}
			analysis := &rekordbox.Analysis{}
			if job.engineID != 0 {
				data, err := readEngineAnalysis(db, root, job.engineID, job.path, job.length)
				if err != nil {
					log.Printf("prolink: background Engine track %d: %v", job.engineID, err)
					return
				}
				analysis = data.Analysis
			}
			if !current() {
				return
			}
			if _, err := writeANLZ(root, job.exportID, job.path, analysis); err != nil {
				log.Printf("prolink: background ANLZ: %v", err)
				return
			}
			if job.artID > 0 && !artDone[job.artID] {
				if _, err := exportEngineArtwork(db, root, job.artID, resolver); err != nil {
					log.Printf("prolink: background artwork %d: %v", job.artID, err)
				}
				artDone[job.artID] = true
			}
			if job.engineID > progress.LastID {
				progress.LastID = job.engineID
			}
			batch++
			if batch >= engineExportBatch || time.Since(batchStart) > 750*time.Millisecond {
				if !current() {
					return
				}
				if err := checkpoint(); err != nil {
					log.Printf("prolink: checkpoint Engine export: %v", err)
					return
				}
				batch = 0
				batchStart = time.Now()
				// Yield between bounded batches, and make an overlapping Sync cancellable.
				select {
				case <-worker.stop:
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if !current() {
			return
		}
		if err := checkpoint(); err != nil {
			log.Printf("prolink: checkpoint Engine export: %v", err)
			return
		}
		// Do not endlessly restart a large pass when playback updates history in WAL.
		// Finish this pass; a later Sync will refresh metadata for the new source.
		if engineSourceStamp(mdbPath) == sourceStamp {
			if err := writeEngineExportFile(filepath.Join(root, "PIONEER/rekordbox/.enginex-stamp"), []byte(stamp), 0644); err != nil {
				log.Printf("prolink: complete Engine export: %v", err)
			}
		}
	}()
}

// generatedMarker holds the size and mtime of the export.pdb this bridge wrote.
const generatedMarker = "PIONEER/rekordbox/.enginex-generated"

// GeneratedExport reports whether root's export.pdb is the one this bridge wrote.
// Once rekordbox exports to the stick again, the file is the DJ's again.
func GeneratedExport(root string) bool {
	st, err := os.Stat(filepath.Join(root, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(root, generatedMarker))
	return err == nil && string(b) == pdbStamp(st)
}

func hasRegularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func hasExportPDB(root string) bool {
	return hasRegularFile(filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")) ||
		hasRegularFile(filepath.Join(root, ".PIONEER", "rekordbox", "export.pdb"))
}

// hasNativeRekordboxExport reports whether root contains any Rekordbox export
// database (PIONEER or .PIONEER; export.pdb, exportLibrary.db, or exportOneLibrary.db)
// not generated by EngineX.
func hasNativeRekordboxExport(root string) bool {
	if GeneratedExport(root) {
		return false
	}
	for _, pdir := range []string{"PIONEER", ".PIONEER"} {
		for _, dbName := range []string{"export.pdb", "exportLibrary.db", "exportOneLibrary.db"} {
			if hasRegularFile(filepath.Join(root, pdir, "rekordbox", dbName)) {
				return true
			}
		}
	}
	return false
}

func pdbStamp(st os.FileInfo) string { return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano()) }

func section(tag string, h int, payload []byte) []byte {
	b := make([]byte, 12)
	copy(b, tag)
	binary.BigEndian.PutUint32(b[4:], uint32(h))
	binary.BigEndian.PutUint32(b[8:], uint32(12+len(payload)))
	return append(b, payload...)
}

func pmai(sections ...[]byte) []byte {
	b := make([]byte, 28)
	copy(b, "PMAI")
	binary.BigEndian.PutUint32(b[4:], 28)
	for _, s := range sections {
		b = append(b, s...)
	}
	binary.BigEndian.PutUint32(b[8:], uint32(len(b)))
	return b
}

func ppthSection(trackPath string) []byte {
	u := utf16.Encode([]rune(trackPath + "\x00"))
	b := make([]byte, 4+2*len(u))
	binary.BigEndian.PutUint32(b[:4], uint32(2*len(u)))
	for i, v := range u {
		binary.BigEndian.PutUint16(b[4+2*i:], v)
	}
	return section("PPTH", 16, b)
}

// Write through a fresh file, never through an existing output symlink.
func writeEngineExportFile(path string, data []byte, mode os.FileMode) (err error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".enginex-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp, path)
}

// Engine key IDs interleave relative major/minor keys; reuse the forward
// mapping rather than maintaining a second, potentially divergent key table.
func engineExportKey(key sql.NullInt64) string {
	if !key.Valid {
		return ""
	}
	for n := 1; n <= 12; n++ {
		for _, mode := range []byte{'A', 'B'} {
			name := fmt.Sprintf("%d%c", n, mode)
			if enginedb.EngineKey(name) == int(key.Int64) {
				return name
			}
		}
	}
	return ""
}

func engineColumn(db *sql.DB, table, column string) bool {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil && name == column {
			return true
		}
	}
	return false
}

type enginePlaylistEntity struct{ id, track, next uint32 }

// Follow entity IDs (not track IDs). Missing tracks, duplicate membership,
// dangling links and cycles must neither reorder the intact chains nor hang.
func enginePlaylistOrder(entities []enginePlaylistEntity) []uint32 {
	byID := make(map[uint32]enginePlaylistEntity, len(entities))
	incoming := make(map[uint32]bool, len(entities))
	for _, e := range entities {
		byID[e.id] = e
		if e.next != 0 {
			incoming[e.next] = true
		}
	}
	seen := make(map[uint32]bool, len(entities))
	var out []uint32
	walk := func(id uint32) {
		for id != 0 && !seen[id] {
			e, ok := byID[id]
			if !ok {
				break
			}
			seen[id] = true
			if e.track != 0 {
				out = append(out, e.track)
			}
			id = e.next
		}
	}
	for _, e := range entities {
		if !incoming[e.id] {
			walk(e.id)
		}
	}
	for _, e := range entities {
		walk(e.id)
	} // deterministic recovery for cycles
	return out
}

const engineBlobLimit = 2 << 20

func finiteEngine(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func engineDouble(b []byte, order binary.ByteOrder) float64 {
	return math.Float64frombits(order.Uint64(b))
}

// Only these four performance fields are read. Qt validation/decompression is
// shared with the importer; SQL substr also bounds the compressed input itself.
// Metadata needs only the beatData audio header, never the waveform payload.
// Qt validation stays shared with enginedb, including its decompression bound.
func engineAudioMetadata(blob []byte) (float64, float64) {
	if len(blob) == 0 {
		return 0, 0
	}
	raw, err := enginedb.QUncompress(blob, engineBlobLimit)
	if err != nil || len(raw) < 17 {
		return 0, 0
	}
	rate, samples := engineDouble(raw, binary.BigEndian), engineDouble(raw[8:], binary.BigEndian)
	if !finiteEngine(rate) || rate < 8000 || rate > 384000 || !finiteEngine(samples) || samples <= 0 || samples/rate > 86400 {
		return 0, 0
	}
	return rate, samples / rate
}

type engineExportAnalysis struct {
	*rekordbox.Analysis
	rate, duration float64
}

func readEngineAnalysis(db *sql.DB, root string, id uint32, path string, length int) (*engineExportAnalysis, error) {
	a := &engineExportAnalysis{Analysis: &rekordbox.Analysis{}, duration: float64(length)}
	if !engineColumn(db, "PerformanceData", "beatData") {
		return a, nil
	}
	var beat, quick, loops, overview []byte
	err := db.QueryRow(`SELECT substr(beatData,1,2097153), substr(quickCues,1,2097153), substr(loops,1,2097153), substr(overviewWaveFormData,1,2097153) FROM PerformanceData WHERE trackId=?`, id).Scan(&beat, &quick, &loops, &overview)
	if err == sql.ErrNoRows {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	rate := 0.0
	duration := float64(length)
	warn := func(field string, err error) {
		log.Printf("prolink: Engine track %d: omit invalid %s: %v", id, field, err)
	}
	if len(beat) > 0 {
		raw, err := enginedb.QUncompress(beat, engineBlobLimit)
		if err == nil {
			var seconds float64
			a.Beats, rate, seconds, err = engineBeats(raw)
			if err == nil {
				duration = seconds
			}
		}
		if err != nil {
			warn("beatData", err)
		}
	}
	// Cue positions are samples. When beatData is absent, use the actual audio
	// header; guessing 44.1 kHz would silently move cues on 48/96 kHz files.
	if rate == 0 {
		if info, err := compat.AudioHeader(filepath.Join(root, path)); err == nil {
			rate = float64(info.SampleRate)
			if info.Duration > 0 {
				duration = info.Duration
			}
		}
	}
	if len(quick) > 0 && rate > 0 {
		raw, err := enginedb.QUncompress(quick, 64<<10)
		if err == nil {
			a.Cues, err = engineQuickCues(raw, rate)
		}
		if err != nil {
			warn("quickCues", err)
		}
	}
	if len(loops) > 0 && rate > 0 {
		var cues []rekordbox.Cue
		var err error
		if len(loops) > 64<<10 {
			err = fmt.Errorf("size limit")
		} else {
			cues, err = engineLoops(loops, rate, a.Cues)
		}
		if err != nil {
			warn("loops", err)
		} else {
			a.Cues = cues
		}
	}
	if len(overview) > 0 {
		raw, err := enginedb.QUncompress(overview, 64<<10)
		if err == nil {
			a.Waveforms, err = engineWaveforms(raw, duration)
		}
		if err != nil {
			warn("overview", err)
		}
	}
	a.rate, a.duration = rate, duration
	return a, nil
}

type engineBeatMarker struct {
	sample float64
	index  int64
}

func engineBeats(raw []byte) ([]rekordbox.Beat, float64, float64, error) {
	bad := func() ([]rekordbox.Beat, float64, float64, error) {
		return nil, 0, 0, fmt.Errorf("invalid Engine beatData")
	}
	if len(raw) < 33 {
		return bad()
	}
	rate, samples := engineDouble(raw, binary.BigEndian), engineDouble(raw[8:], binary.BigEndian)
	if !finiteEngine(rate) || rate < 8000 || rate > 384000 || !finiteEngine(samples) || samples <= 0 || samples/rate > 86400 || raw[16] > 1 {
		return bad()
	}
	var grids [2][]engineBeatMarker
	off := 17
	for g := range grids {
		if len(raw)-off < 8 {
			return bad()
		}
		n := binary.BigEndian.Uint64(raw[off:])
		off += 8
		if n > uint64((len(raw)-off)/24) || n > 32768 {
			return bad()
		}
		for i := uint64(0); i < n; i++ {
			m := engineBeatMarker{engineDouble(raw[off:], binary.LittleEndian), int64(binary.LittleEndian.Uint64(raw[off+8:]))}
			off += 24
			if !finiteEngine(m.sample) || math.Abs(m.sample) > 1e12 || m.index < -1000000 || m.index > 1000000 {
				return bad()
			}
			if len(grids[g]) > 0 {
				prev := grids[g][len(grids[g])-1]
				if m.sample <= prev.sample || m.index <= prev.index {
					return bad()
				}
			}
			grids[g] = append(grids[g], m)
		}
	}
	grid := grids[1]
	if len(grid) < 2 {
		grid = grids[0]
	}
	if raw[16] == 0 || len(grid) < 2 {
		return nil, rate, samples / rate, nil
	}
	first, last := grid[0], grid[len(grid)-1]
	firstStep := (grid[1].sample - first.sample) / float64(grid[1].index-first.index)
	lastStep := (last.sample - grid[len(grid)-2].sample) / float64(last.index-grid[len(grid)-2].index)
	if !finiteEngine(firstStep) || !finiteEngine(lastStep) || firstStep < 60*rate/655.35 || lastStep < 60*rate/655.35 {
		return bad()
	}
	startFloat := math.Ceil(float64(first.index) - first.sample/firstStep)
	endFloat := math.Floor(float64(last.index) + (samples-last.sample)/lastStep)
	if !finiteEngine(startFloat) || !finiteEngine(endFloat) || math.Abs(startFloat) > 2000000 || math.Abs(endFloat) > 2000000 || endFloat < startFloat || endFloat-startFloat > 100000 {
		return bad()
	}
	start, end := int64(startFloat), int64(endFloat)
	beats := make([]rekordbox.Beat, 0, end-start+1)
	segment := 0
	for index := start; index <= end; index++ {
		for segment+2 < len(grid) && index >= grid[segment+1].index {
			segment++
		}
		left, right := grid[segment], grid[segment+1]
		step := (right.sample - left.sample) / float64(right.index-left.index)
		bpm := 60 * rate / step
		sample := left.sample + float64(index-left.index)*step
		if sample < 0 || sample > samples {
			continue
		}
		if !finiteEngine(bpm) || bpm <= 0 || bpm > 655.35 {
			return bad()
		}
		ms := uint32(math.Round(sample * 1000 / rate))
		if len(beats) > 0 && ms <= beats[len(beats)-1].TimeMS {
			return bad()
		}
		beats = append(beats, rekordbox.Beat{Number: uint16(((index%4 + 4) % 4) + 1), BPM: bpm, TimeMS: ms})
	}
	return beats, rate, samples / rate, nil
}

func engineCueMS(sample, rate float64) (uint32, bool) {
	ms := sample * 1000 / rate
	if !finiteEngine(ms) || ms < 0 || ms > math.MaxUint32 {
		return 0, false
	}
	return uint32(math.Round(ms)), true
}

func engineQuickCues(raw []byte, rate float64) ([]rekordbox.Cue, error) {
	bad := func() ([]rekordbox.Cue, error) { return nil, fmt.Errorf("invalid Engine quickCues") }
	if len(raw) < 8 || binary.BigEndian.Uint64(raw) != 8 {
		return bad()
	}
	var cues []rekordbox.Cue
	off := 8
	for slot := 1; slot <= 8; slot++ {
		if off >= len(raw) {
			return bad()
		}
		n := int(raw[off])
		off++
		if len(raw)-off < n+12 {
			return bad()
		}
		name := string(raw[off : off+n])
		off += n
		sample := engineDouble(raw[off:], binary.BigEndian)
		color := [4]byte{raw[off+8], raw[off+9], raw[off+10], raw[off+11]}
		off += 12
		if sample == -1 {
			continue
		}
		ms, ok := engineCueMS(sample, rate)
		if !ok {
			return bad()
		}
		cues = append(cues, rekordbox.Cue{HotCue: uint32(slot), TimeMS: ms, Name: name, Color: color})
	}
	if len(raw)-off != 17 || raw[off+8] > 1 {
		return bad()
	}
	// main_cue is the selected memory cue. default_cue is an alternate/reset
	// position, not a second memory-cue list, and must not create a spurious cue.
	sample := engineDouble(raw[off:], binary.BigEndian)
	if sample != -1 {
		ms, ok := engineCueMS(sample, rate)
		if !ok {
			return bad()
		}
		cues = append(cues, rekordbox.Cue{TimeMS: ms})
	}
	return cues, nil
}

func engineLoops(raw []byte, rate float64, cues []rekordbox.Cue) ([]rekordbox.Cue, error) {
	bad := func() ([]rekordbox.Cue, error) { return nil, fmt.Errorf("invalid Engine loops") }
	if len(raw) < 8 || binary.LittleEndian.Uint64(raw) > 8 {
		return bad()
	}
	cues = append([]rekordbox.Cue(nil), cues...)
	off := 8
	for slot := 1; slot <= int(raw[0]); slot++ {
		if off >= len(raw) {
			return bad()
		}
		n := int(raw[off])
		off++
		if len(raw)-off < n+22 {
			return bad()
		}
		name := string(raw[off : off+n])
		off += n
		start, end := engineDouble(raw[off:], binary.LittleEndian), engineDouble(raw[off+8:], binary.LittleEndian)
		setStart, setEnd := raw[off+16], raw[off+17]
		color := [4]byte{raw[off+18], raw[off+19], raw[off+20], raw[off+21]}
		off += 22
		if setStart > 1 || setEnd > 1 {
			return bad()
		}
		if setStart == 0 || setEnd == 0 {
			continue
		}
		ms, ok := engineCueMS(start, rate)
		endMS, okEnd := engineCueMS(end, rate)
		if !ok || !okEnd || endMS <= ms {
			return bad()
		}
		c := rekordbox.Cue{TimeMS: ms, EndMS: endMS, Loop: true, Name: name, Color: color}
		merged := false
		for i := range cues {
			if cues[i].HotCue == uint32(slot) && cues[i].TimeMS == ms {
				c.HotCue = uint32(slot)
				cues[i] = c
				merged = true
				break
			}
		}
		if !merged {
			cues = append(cues, c)
		} // Engine loop slots are independent of hot cues.
	}
	if off != len(raw) {
		return bad()
	}
	return cues, nil
}

// overviewWaveFormData is an overview, not full-resolution PCM. Resample its
// measured low/mid/high bands onto the documented 150 entries/second timeline;
// this preserves timing/variation but cannot recover detail absent in m.db.
func engineWaveforms(raw []byte, duration float64) ([]rekordbox.Waveform, error) {
	if len(raw) < 24 {
		return nil, fmt.Errorf("invalid Engine overview")
	}
	n := binary.BigEndian.Uint64(raw[8:])
	span := engineDouble(raw[16:], binary.BigEndian)
	if n == 0 || n > 16384 || binary.BigEndian.Uint64(raw) != n || uint64(len(raw)) != 24+(n+1)*3 || !finiteEngine(span) || span <= 0 {
		return nil, fmt.Errorf("invalid Engine overview bins")
	}
	if !finiteEngine(duration) || duration <= 0 || duration > 3600 {
		return nil, nil
	}
	count := int(math.Ceil(duration * 150))
	rgb := make([]byte, count*2)
	bands := make([]byte, count*3)
	for i := 0; i < count; i++ {
		lo, hi := uint64(i)*n/uint64(count), uint64(i+1)*n/uint64(count)
		if hi <= lo {
			hi = lo + 1
		}
		var peak [3]byte
		for j := lo; j < hi && j < n; j++ {
			for c := range peak {
				peak[c] = max(peak[c], raw[24+j*3+uint64(c)])
			}
		}
		// PWV7 byte order is mid, high, low (Deep Symmetry ANLZ documentation).
		copy(bands[3*i:], []byte{peak[1], peak[2], peak[0]})
		// PWV5 packs 3-bit RGB and 5-bit height. Normalize the Engine band scales
		// used by the importer, rather than copying incompatible overview bytes.
		r, g, b := min(1, float64(peak[0])*0.0066), min(1, float64(peak[1])*0.0035), min(1, float64(peak[2])*0.0016)
		height := max(r, g, b)
		var word uint16
		if height > 0 {
			word = uint16(math.Round(r/height*7))<<13 | uint16(math.Round(g/height*7))<<10 | uint16(math.Round(b/height*7))<<7 | uint16(math.Round(height*31))<<2
		}
		binary.BigEndian.PutUint16(rgb[2*i:], word)
	}
	return []rekordbox.Waveform{{Tag: "PWV5", EntryBytes: 2, Data: rgb}, {Tag: "PWV7", EntryBytes: 3, Data: bands}}, nil
}

func pqtzSection(beats []rekordbox.Beat) []byte {
	payload := make([]byte, 12+8*len(beats))
	binary.BigEndian.PutUint32(payload[4:], 0x00800000)
	binary.BigEndian.PutUint32(payload[8:], uint32(len(beats)))
	for i, beat := range beats {
		p := payload[12+8*i:]
		binary.BigEndian.PutUint16(p, beat.Number)
		binary.BigEndian.PutUint16(p[2:], uint16(math.Round(beat.BPM*100)))
		binary.BigEndian.PutUint32(p[4:], beat.TimeMS)
	}
	return section("PQTZ", 24, payload)
}

func waveformSection(w rekordbox.Waveform) []byte {
	payload := make([]byte, 12+len(w.Data))
	binary.BigEndian.PutUint32(payload, w.EntryBytes)
	binary.BigEndian.PutUint32(payload[4:], uint32(len(w.Data))/w.EntryBytes)
	unknown := uint32(0x00960305)
	if w.Tag == "PWV7" {
		unknown = 0x00960000
	}
	binary.BigEndian.PutUint32(payload[8:], unknown)
	copy(payload[12:], w.Data)
	return section(w.Tag, 24, payload)
}

// Public rekordbox color-code table documented by Deep Symmetry/Beat Link,
// CueList.findRekordboxColor. Explicit LED RGB remains byte-exact; the nearest
// palette code prevents players from interpreting every cue as default green.
var enginePioneerCuePalette = [][3]byte{
	{0x30, 0x5a, 0xff},
	{0x50, 0x73, 0xff},
	{0x50, 0x8c, 0xff},
	{0x50, 0xa0, 0xff},
	{0x50, 0xb4, 0xff},
	{0x50, 0xb0, 0xf2},
	{0x50, 0xae, 0xe8},
	{0x45, 0xac, 0xdb},
	{0x00, 0xe0, 0xff},
	{0x19, 0xda, 0xf0},
	{0x32, 0xd2, 0xe6},
	{0x21, 0xb4, 0xb9},
	{0x20, 0xaa, 0xa0},
	{0x1f, 0xa3, 0x92},
	{0x19, 0xa0, 0x8c},
	{0x14, 0xa5, 0x84},
	{0x14, 0xaa, 0x7d},
	{0x10, 0xb1, 0x76},
	{0x30, 0xd2, 0x6e},
	{0x37, 0xde, 0x5a},
	{0x3c, 0xeb, 0x50},
	{0x28, 0xe2, 0x14},
	{0x7d, 0xc1, 0x3d},
	{0x8c, 0xc8, 0x32},
	{0x9b, 0xd7, 0x23},
	{0xa5, 0xe1, 0x16},
	{0xa5, 0xdc, 0x0a},
	{0xaa, 0xd2, 0x08},
	{0xb4, 0xc8, 0x05},
	{0xb4, 0xbe, 0x04},
	{0xba, 0xb4, 0x04},
	{0xc3, 0xaf, 0x04},
	{0xe1, 0xaa, 0x00},
	{0xff, 0xa0, 0x00},
	{0xff, 0x96, 0x00},
	{0xff, 0x8c, 0x00},
	{0xff, 0x75, 0x00},
	{0xe0, 0x64, 0x1b},
	{0xe0, 0x46, 0x1e},
	{0xe0, 0x30, 0x1e},
	{0xe0, 0x28, 0x23},
	{0xe6, 0x28, 0x28},
	{0xff, 0x37, 0x6f},
	{0xff, 0x2d, 0x6f},
	{0xff, 0x12, 0x7b},
	{0xf5, 0x1e, 0x8c},
	{0xeb, 0x2d, 0xa0},
	{0xe6, 0x37, 0xb4},
	{0xde, 0x44, 0xcf},
	{0xde, 0x44, 0x8d},
	{0xe6, 0x30, 0xb4},
	{0xe6, 0x19, 0xdc},
	{0xe6, 0x00, 0xff},
	{0xdc, 0x00, 0xff},
	{0xcc, 0x00, 0xff},
	{0xb4, 0x32, 0xff},
	{0xb9, 0x3c, 0xff},
	{0xc5, 0x42, 0xff},
	{0xaa, 0x5a, 0xff},
	{0xaa, 0x72, 0xff},
	{0x82, 0x72, 0xff},
	{0x64, 0x73, 0xff},
}

func enginePioneerCueColor(c rekordbox.Cue) byte {
	if c.ColorCode != 0 || c.Color == [4]byte{} {
		return c.ColorCode
	}
	best, distance := byte(1), int(^uint(0)>>1)
	for i, rgb := range enginePioneerCuePalette {
		d := 0
		for j := range rgb {
			delta := int(rgb[j]) - int(c.Color[j+1])
			d += delta * delta
		}
		if d < distance {
			best, distance = byte(i+1), d
		}
	}
	return best
}

// Emit separate memory/hot lists. PCOB supports older players, PCO2 carries
// the UTF-16 labels and explicit colors used by NXS2/3000.
func cueSections(cues []rekordbox.Cue, extended bool) [][]byte {
	var sections [][]byte
	for kind := uint32(0); kind < 2; kind++ {
		var selected []rekordbox.Cue
		for _, c := range cues {
			if (c.HotCue > 0) == (kind == 1) {
				selected = append(selected, c)
			}
		}
		sort.SliceStable(selected, func(i, j int) bool {
			if kind == 1 {
				return selected[i].HotCue < selected[j].HotCue
			}
			return selected[i].TimeMS < selected[j].TimeMS
		})
		header := 24
		tag := "PCOB"
		if extended {
			header = 20
			tag = "PCO2"
		}
		payload := make([]byte, header-12)
		binary.BigEndian.PutUint32(payload, kind)
		countAt := 6
		if extended {
			countAt = 4
		} else {
			binary.BigEndian.PutUint16(payload[4:], 0xffff)
			binary.BigEndian.PutUint32(payload[8:], uint32(len(selected)))
		}
		binary.BigEndian.PutUint16(payload[countAt:], uint16(len(selected)))
		for i, c := range selected {
			typ := byte(1)
			end := uint32(0xffffffff)
			if c.Loop {
				typ = 2
				end = c.EndMS
			}
			if !extended {
				p := make([]byte, 56)
				copy(p, "PCPT")
				binary.BigEndian.PutUint32(p[4:], 28)
				binary.BigEndian.PutUint32(p[8:], 56)
				binary.BigEndian.PutUint32(p[12:], c.HotCue)
				binary.BigEndian.PutUint32(p[20:], 0x100000)
				first, last := uint16(i-1), uint16(i+1)
				if i == 0 {
					first = 0xffff
				}
				if i == len(selected)-1 {
					last = 0xffff
				}
				binary.BigEndian.PutUint16(p[24:], first)
				binary.BigEndian.PutUint16(p[26:], last)
				p[28] = typ
				p[30] = 3
				p[31] = 0xe8
				binary.BigEndian.PutUint32(p[32:], c.TimeMS)
				binary.BigEndian.PutUint32(p[36:], end)
				payload = append(payload, p...)
			} else {
				var label []uint16
				if c.Name != "" {
					label = utf16.Encode([]rune(c.Name + "\x00"))
				}
				p := make([]byte, 48+2*len(label))
				copy(p, "PCP2")
				binary.BigEndian.PutUint32(p[4:], 16)
				binary.BigEndian.PutUint32(p[8:], uint32(len(p)))
				binary.BigEndian.PutUint32(p[12:], c.HotCue)
				p[16] = typ
				p[18] = 3
				p[19] = 0xe8
				binary.BigEndian.PutUint32(p[20:], c.TimeMS)
				binary.BigEndian.PutUint32(p[24:], end)
				p[29] = 1
				binary.BigEndian.PutUint32(p[40:], uint32(2*len(label)))
				for j, v := range label {
					binary.BigEndian.PutUint16(p[44+2*j:], v)
				}
				q := 44 + 2*len(label)
				p[q] = enginePioneerCueColor(c)
				copy(p[q+1:], c.Color[1:])
				payload = append(payload, p...)
			}
		}
		sections = append(sections, section(tag, header, payload))
	}
	return sections
}

// Covers are copied to the generated Pioneer tree; the Engine database and
// Engine Library/Artwork are never changed. DecodeConfig bounds image work.
func exportEngineArtwork(db *sql.DB, root string, id int64, resolver *compat.Resolver) (string, error) {
	var data []byte
	var hash any
	err := db.QueryRow(`SELECT hash, substr(albumArt,1,4194305) FROM AlbumArt WHERE id=?`, id).Scan(&hash, &data)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		// Newer Engine libraries can keep only a SHA-1 in SQLite and the JPEG
		// in Artwork. Accept only a hash, never a database-provided arbitrary path.
		var sum []byte
		switch h := hash.(type) {
		case []byte:
			if len(h) == 20 {
				sum = h
			}
		case string:
			if len(h) == 40 {
				sum, _ = hex.DecodeString(h)
			}
		}
		if len(sum) != 20 {
			return "", nil
		}
		if resolver == nil {
			resolver, err = compat.NewResolver(root)
			if err != nil {
				return "", err
			}
		}
		rel, err := resolver.Resolve("Engine Library/Artwork/" + base64.RawURLEncoding.EncodeToString(sum) + ".jpg")
		if err != nil {
			return "", nil
		}
		file, err := os.Open(resolver.FullPath(rel))
		if err != nil {
			return "", err
		}
		data, err = io.ReadAll(io.LimitReader(file, (4<<20)+1))
		closeErr := file.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	if len(data) == 0 {
		return "", nil
	}
	if len(data) > 4<<20 {
		return "", fmt.Errorf("artwork %d exceeds size limit", id)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("artwork %d: %w", id, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 {
		return "", fmt.Errorf("artwork %d exceeds dimension limit", id)
	}
	if format != "jpeg" {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return "", err
		}
		var out bytes.Buffer
		if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 90}); err != nil {
			return "", err
		}
		data = out.Bytes()
	}
	dir, err := compat.SafeOutputDir(root, "PIONEER/Artwork")
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("A%d.jpg", id)
	if err := writeEngineExportFile(filepath.Join(dir, name), data, 0644); err != nil {
		return "", err
	}
	return engineArtworkPath(id), nil
}

func buildArtistRow(id uint32, name string) []byte {
	b := make([]byte, 10)
	binary.LittleEndian.PutUint32(b[4:], id)
	b[9] = 10
	return append(b, rekordbox.EncodeDeviceSQLString(name)...)
}

func buildKeyRow(id uint32, name string) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[:4], id)
	binary.LittleEndian.PutUint32(b[4:], id)
	return append(b, rekordbox.EncodeDeviceSQLString(name)...)
}

func buildPlaylistRow(id, parent, order uint32, name string, folder bool) []byte {
	b := make([]byte, 20)
	binary.LittleEndian.PutUint32(b[0:], parent)
	binary.LittleEndian.PutUint32(b[8:], order)
	binary.LittleEndian.PutUint32(b[12:], id)
	if folder {
		binary.LittleEndian.PutUint32(b[16:], 1)
	}
	return append(b, rekordbox.EncodeDeviceSQLString(name)...)
}

func buildPlaylistEntryRow(order, trackID, playlistID uint32) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b[0:], order)
	binary.LittleEndian.PutUint32(b[4:], trackID)
	binary.LittleEndian.PutUint32(b[8:], playlistID)
	return b
}

func buildTrackRow(t rekordbox.Track, artistID, keyID uint32) []byte {
	b := make([]byte, 136)
	sampleRate := t.SampleRate
	if sampleRate < 0 {
		sampleRate = 0
	}
	fileSize := t.FileSize
	if fileSize <= 0 {
		fileSize = 1048576
	}
	bitrate := t.Bitrate
	if bitrate <= 0 {
		bitrate = 320
	}
	year := t.Year
	if year <= 0 {
		year = 2026
	}
	bitDepth := t.BitDepth
	if bitDepth <= 0 {
		bitDepth = 24
	}

	binary.LittleEndian.PutUint32(b[8:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(b[16:], uint32(fileSize))
	if t.ArtworkPath != "" {
		binary.LittleEndian.PutUint32(b[28:], t.ID)
	}
	binary.LittleEndian.PutUint32(b[32:], keyID)
	binary.LittleEndian.PutUint32(b[48:], uint32(bitrate))
	binary.LittleEndian.PutUint32(b[56:], uint32(math.Round(t.BPM*100)))
	binary.LittleEndian.PutUint32(b[68:], artistID)
	binary.LittleEndian.PutUint32(b[72:], t.ID)
	binary.LittleEndian.PutUint16(b[80:], uint16(year))
	binary.LittleEndian.PutUint16(b[82:], uint16(bitDepth))
	binary.LittleEndian.PutUint16(b[84:], uint16(math.Round(t.Duration)))
	b[89] = 3

	for i := 0; i < 21; i++ {
		s := ""
		switch i {
		case 14:
			s = t.AnalysisPath
		case 16:
			s = t.Comment
		case 17:
			s = t.Title
		case 20:
			s = t.Path
		}
		binary.LittleEndian.PutUint16(b[94+2*i:], uint16(len(b)))
		b = append(b, rekordbox.EncodeDeviceSQLString(s)...)
	}
	return b
}

func buildPDB(tracks []rekordbox.Track, playlists []rekordbox.Playlist) ([]byte, error) {
	library := rekordbox.Library{Tracks: tracks, Playlists: playlists}
	if err := library.Validate(); err != nil {
		return nil, fmt.Errorf("validate export library: %w", err)
	}
	artistMap := make(map[string]uint32)
	var artistRows [][]byte
	var artistNames []string
	for _, t := range tracks {
		name := t.Artist
		if name == "" {
			name = "Unknown Artist"
		}
		if _, exists := artistMap[name]; !exists {
			artistNames = append(artistNames, name)
			artistMap[name] = 0
		}
	}
	sort.Strings(artistNames)
	for i, name := range artistNames {
		id := uint32(i + 1)
		artistMap[name] = id
		artistRows = append(artistRows, buildArtistRow(id, name))
	}
	if len(artistRows) == 0 {
		artistRows = append(artistRows, buildArtistRow(1, "Unknown Artist"))
	}

	keyMap := make(map[string]uint32)
	var keyRows [][]byte
	var keyNames []string
	for _, t := range tracks {
		k := t.Key
		if k == "" {
			continue
		}
		if _, exists := keyMap[k]; !exists {
			keyNames = append(keyNames, k)
			keyMap[k] = 0
		}
	}
	sort.Strings(keyNames)
	for i, name := range keyNames {
		id := uint32(i + 1)
		keyMap[name] = id
		keyRows = append(keyRows, buildKeyRow(id, name))
	}

	var trackRows [][]byte
	for _, t := range tracks {
		aName := t.Artist
		if aName == "" {
			aName = "Unknown Artist"
		}
		aID := artistMap[aName]
		if aID == 0 {
			aID = 1
		}
		kID := keyMap[t.Key]
		trackRows = append(trackRows, buildTrackRow(t, aID, kID))
	}

	var playlistRows [][]byte
	var entryRows [][]byte
	for _, p := range playlists {
		playlistRows = append(playlistRows, buildPlaylistRow(p.ID, p.ParentID, p.SortOrder, p.Name, p.Folder))
		for j, trackID := range p.Tracks {
			entryRows = append(entryRows, buildPlaylistEntryRow(uint32(j+1), trackID, p.ID))
		}
	}

	type tableSpec struct {
		typ  uint32
		rows [][]byte
	}
	var artworkRows [][]byte
	for _, t := range tracks {
		if t.ArtworkPath == "" {
			continue
		}
		row := make([]byte, 4)
		binary.LittleEndian.PutUint32(row, t.ID)
		artworkRows = append(artworkRows, append(row, rekordbox.EncodeDeviceSQLString(t.ArtworkPath)...))
	}
	tables := []tableSpec{
		{0, trackRows},
		{2, artistRows},
		{5, keyRows},
		{7, playlistRows},
		{8, entryRows},
		{13, artworkRows},
		{16, [][]byte{[]byte("opaque column definition")}},
	}

	const pageSize = 4096
	pages := [][]byte{make([]byte, pageSize)}
	type tablePageInfo struct {
		first, last uint32
	}
	tablePages := make([]tablePageInfo, len(tables))

	for ti, t := range tables {
		var curPage []byte
		var curPageNum, firstPageNum uint32
		var curRowCount int
		var curOff int

		startNewPage := func() {
			curPageNum = uint32(len(pages))
			if firstPageNum == 0 {
				firstPageNum = curPageNum
			}
			curPage = make([]byte, pageSize)
			binary.LittleEndian.PutUint32(curPage[4:], curPageNum)
			binary.LittleEndian.PutUint32(curPage[8:], t.typ)
			curPage[27] = 0x24
			curRowCount = 0
			curOff = 40
			pages = append(pages, curPage)
		}

		if len(t.rows) == 0 {
			startNewPage()
			binary.LittleEndian.PutUint16(curPage[24:], 0)
			binary.LittleEndian.PutUint16(curPage[30:], 0)
		} else {
			startNewPage()
			for _, row := range t.rows {
				nextCount := curRowCount + 1
				indexBytes := (nextCount / 16) * 36
				if rem := nextCount % 16; rem != 0 {
					indexBytes += 4 + 2*rem
				}
				heapEnd := pageSize - indexBytes
				if curOff+len(row) > heapEnd {
					nextPageNum := uint32(len(pages))
					binary.LittleEndian.PutUint32(curPage[12:], nextPageNum)
					binary.LittleEndian.PutUint16(curPage[24:], uint16(curRowCount))
					binary.LittleEndian.PutUint16(curPage[30:], uint16(curOff-40))
					startNewPage()
				}

				copy(curPage[curOff:], row)
				rowOff := curOff - 40
				base := pageSize - (curRowCount/16)*36
				binary.LittleEndian.PutUint16(curPage[base-6-2*(curRowCount%16):], uint16(rowOff))
				flags := binary.LittleEndian.Uint16(curPage[base-4:])
				binary.LittleEndian.PutUint16(curPage[base-4:], flags|(1<<uint(curRowCount%16)))

				curOff += len(row)
				curRowCount++
			}
			binary.LittleEndian.PutUint16(curPage[24:], uint16(curRowCount))
			binary.LittleEndian.PutUint16(curPage[30:], uint16(curOff-40))
		}

		tablePages[ti] = tablePageInfo{first: firstPageNum, last: curPageNum}
	}

	header := pages[0]
	binary.LittleEndian.PutUint32(header[4:], pageSize)
	binary.LittleEndian.PutUint32(header[8:], uint32(len(tables)))
	binary.LittleEndian.PutUint32(header[12:], uint32(len(pages)))
	binary.LittleEndian.PutUint32(header[20:], 1)

	for i, tp := range tablePages {
		h := header[28+i*16:]
		binary.LittleEndian.PutUint32(h[0:], tables[i].typ)
		binary.LittleEndian.PutUint32(h[4:], 0)
		binary.LittleEndian.PutUint32(h[8:], tp.first)
		binary.LittleEndian.PutUint32(h[12:], tp.last)
	}

	out := make([]byte, len(pages)*pageSize)
	for i, p := range pages {
		copy(out[i*pageSize:], p)
	}
	return out, nil
}

// Where EngineX OS keeps streaming downloads; CDJs see them as /EngineX_Stream/<name>.
// Variables so tests can point them at a temporary tree.
var (
	StreamDirs       = []string{"/data/sc5000-custom/Stream", "/media/sc5000-sd-cache/Music/Stream"}
	InternalEngineDB = "/data/AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db"
	// StreamExportRoot holds the export.pdb of a deck with no USB library, so its
	// streaming downloads still reach the CDJs.
	StreamExportRoot = "/data/sc5000-custom/prolink-export"
)

const (
	StreamPrefix = "EngineX_Stream"
	StreamFolder = "EngineX Streaming"
)

var streamExts = map[string]bool{".mp3": true, ".flac": true, ".wav": true, ".m4a": true, ".aiff": true, ".aif": true}

// Display names for the provider in a .sources.json key ("deezer:123").
var streamPlatforms = map[string]string{"deezer": "Deezer", "soundcloud": "SoundCloud", "netease": "NetEase", "qq": "QQ Music", "qqmusic": "QQ Music",
	"applemusic": "Apple Music", "spotify": "Spotify", "beatport": "Beatport", "tidal": "TIDAL"}

type streamFile struct {
	name, platform string
	size           int64
	mod            int64
}

// scanStreams lists the audio files directly in StreamDirs (the first directory
// wins on a repeated name, as in StreamFile), sorted by name.
func scanStreams() []streamFile {
	seen := map[string]bool{}
	var out []streamFile
	for _, dir := range StreamDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		platform := map[string]string{}
		var sources map[string]string
		if b, err := os.ReadFile(filepath.Join(dir, ".sources.json")); err == nil && json.Unmarshal(b, &sources) == nil {
			for key, name := range sources {
				if p, _, ok := strings.Cut(key, ":"); ok && streamPlatforms[p] != "" {
					platform[name] = streamPlatforms[p]
				}
			}
		}
		for _, e := range entries {
			name := e.Name()
			if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || !streamExts[strings.ToLower(filepath.Ext(name))] || seen[name] {
				continue
			}
			info, err := e.Info()
			if err != nil || info.Size() == 0 {
				continue
			}
			seen[name] = true
			p := platform[name]
			if p == "" {
				p = "Downloaded Stream"
			}
			out = append(out, streamFile{name: name, platform: p, size: info.Size(), mod: info.ModTime().UnixNano()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func streamStamp(streams []streamFile) string {
	h := fnv.New64a()
	for _, s := range streams {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\n", s.name, s.platform, s.size, s.mod)
	}
	return fmt.Sprintf("streams:%d:%x", len(streams), h.Sum64())
}

// StreamFile is the file behind /EngineX_Stream/<rel>, or "" if none.
func StreamFile(p string) string {
	rel, ok := SafeRel(p)
	first, rest, _ := strings.Cut(filepath.ToSlash(rel), "/")
	if !ok || first != StreamPrefix || rest == "" {
		return ""
	}
	for _, dir := range StreamDirs {
		full := filepath.Join(dir, filepath.FromSlash(rest))
		if st, err := os.Lstat(full); err == nil && st.Mode().IsRegular() {
			return full
		}
	}
	return ""
}

// dropStaleStream removes the streaming folder (it is rebuilt) and the
// streaming tracks whose download was evicted, so a CDJ never lists a file
// that is gone.
func dropStaleStream(lib *rekordbox.Library, streams []streamFile) {
	present := map[string]bool{}
	for _, s := range streams {
		present["/"+StreamPrefix+"/"+s.name] = true
	}
	gone := map[uint32]bool{}
	tracks := lib.Tracks[:0]
	for _, t := range lib.Tracks {
		p := "/" + strings.TrimPrefix(filepath.ToSlash(filepath.Clean(t.Path)), "/")
		if strings.HasPrefix(p, "/"+StreamPrefix+"/") && !present[p] {
			gone[t.ID] = true
			continue
		}
		tracks = append(tracks, t)
	}
	lib.Tracks = tracks
	folders := map[uint32]bool{}
	for _, p := range lib.Playlists {
		if p.ParentID == 0 && p.Folder && p.Name == StreamFolder {
			folders[p.ID] = true
		}
	}
	lists := lib.Playlists[:0]
	for _, p := range lib.Playlists {
		if folders[p.ID] || folders[p.ParentID] {
			continue
		}
		kept := p.Tracks[:0]
		for _, id := range p.Tracks {
			if !gone[id] {
				kept = append(kept, id)
			}
		}
		p.Tracks = kept
		lists = append(lists, p)
	}
	lib.Playlists = lists
}

type engineMeta struct {
	title, artist string
	bpm           float64
	length        int
}

// internalMeta reads titles, artists, BPM and length from the deck's own
// Engine library by file name, read-only: Engine owns that database.
func internalMeta() map[string]engineMeta {
	out := map[string]engineMeta{}
	if st, err := os.Stat(InternalEngineDB); err != nil || !st.Mode().IsRegular() {
		return out
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: InternalEngineDB}).EscapedPath()+"?mode=ro")
	if err != nil {
		return out
	}
	defer db.Close()
	rows, err := db.Query(`SELECT COALESCE(filename, ''), COALESCE(title, ''), COALESCE(artist, ''), COALESCE(bpmAnalyzed, 0), COALESCE(bpm, 0), COALESCE(length, 0) FROM Track WHERE filename IS NOT NULL AND filename != ''`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var m engineMeta
		var bpm int
		if rows.Scan(&name, &m.title, &m.artist, &m.bpm, &bpm, &m.length) != nil {
			continue
		}
		if m.bpm <= 0 {
			m.bpm = float64(bpm)
		}
		out[strings.ToLower(name)] = m
	}
	return out
}

// streamMeta is the export row of a download: Engine's metadata when Engine
// has the file, else "Artist - Title" from the file name and the audio header.
func streamMeta(s streamFile, m engineMeta) rekordbox.Track {
	base := strings.TrimSuffix(s.name, filepath.Ext(s.name))
	if m.title == "" {
		m.title = base
		if a, t, ok := strings.Cut(base, " - "); ok && m.artist == "" {
			m.artist, m.title = strings.TrimSpace(a), strings.TrimSpace(t)
		}
	}
	t := rekordbox.Track{Title: m.title, Artist: m.artist, BPM: m.bpm, Duration: float64(m.length), Comment: s.platform,
		SampleRate: 44100, BitDepth: 16, Bitrate: 320, Year: 2026, FileSize: s.size}
	if info, err := compat.AudioHeader(StreamFile("/" + StreamPrefix + "/" + s.name)); err == nil {
		t.SampleRate, t.BitDepth, t.Bitrate = info.SampleRate, info.BitDepth, info.Bitrate
		if t.Duration <= 0 {
			t.Duration = math.Round(info.Duration)
		}
	}
	if t.Duration <= 0 {
		// shortcut: mp3/m4a length is estimated at 320 kbps, read the frame headers if CDJs show wrong times.
		t.Duration = math.Max(1, math.Round(float64(s.size)*8/320000))
	}
	if t.BPM <= 0 {
		t.BPM = 120 // no grid yet: Engine has not analysed the download
	}
	return t
}

// writeANLZ writes the analysis files of a newly exported track (beat grid and
// waveforms), replacing any left by an evicted track that had its id, and
// returns the path export.pdb points to.
func writeANLZ(root string, id uint32, trackPath string, a *rekordbox.Analysis) (string, error) {
	anlzRel := fmt.Sprintf("PIONEER/USBANLZ/P%03d", id)
	dir, err := compat.SafeOutputDir(root, anlzRel)
	if err != nil {
		return "", fmt.Errorf("mkdir anlz dir: %w", err)
	}
	dat := [][]byte{ppthSection(trackPath), pqtzSection(a.Beats)}
	dat = append(dat, cueSections(a.Cues, false)...)
	ext := [][]byte{ppthSection(trackPath)}
	ext = append(ext, cueSections(a.Cues, true)...)
	two := [][]byte{ppthSection(trackPath)}
	for _, w := range a.Waveforms {
		if w.Tag == "PWV5" {
			ext = append(ext, waveformSection(w))
		}
		if w.Tag == "PWV7" {
			two = append(two, waveformSection(w))
		}
	}
	for name, body := range map[string][]byte{"ANLZ0000.DAT": pmai(dat...), "ANLZ0000.EXT": pmai(ext...), "ANLZ0000.2EX": pmai(two...)} {
		if err := writeEngineExportFile(filepath.Join(dir, name), body, 0644); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
	}
	return "/" + anlzRel + "/ANLZ0000.DAT", nil
}
