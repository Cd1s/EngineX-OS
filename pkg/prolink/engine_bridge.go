package prolink

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"

	_ "modernc.org/sqlite"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// SyncEngineToPioneerExport writes root's export.pdb so that Pioneer CDJ players
// can browse and play over Pro DJ Link (dbserver + NFS): the Engine library in
// <root>/Engine Library/Database2/m.db, if any, and the EngineX OS streaming
// downloads (StreamDirs) as /EngineX_Stream/<name> in an "EngineX Streaming"
// folder. A real rekordbox export is left alone; an unchanged source is a no-op.
func SyncEngineToPioneerExport(root string) error {
	mdbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	fi, err := os.Stat(mdbPath)
	hasMDB := err == nil && fi.Mode().IsRegular()
	streams := scanStreams()
	if !hasMDB && len(streams) == 0 {
		return nil
	}

	pdbPath := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
	stampPath := filepath.Join(root, "PIONEER", "rekordbox", ".enginex-stamp")
	currentStamp := streamStamp(streams)
	if hasMDB {
		currentStamp = fmt.Sprintf("%d:%d|%s", fi.Size(), fi.ModTime().UnixNano(), currentStamp)
	}

	// A real rekordbox export is the DJ's own: only an export this bridge wrote is rebuilt.
	if _, err := os.Stat(pdbPath); err == nil && !GeneratedExport(root) {
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

	trackIDMap := make(map[uint32]uint32)
	trackMap := make(map[uint32]bool)
	if existingLib != nil {
		for _, t := range existingLib.Tracks {
			trackMap[t.ID] = true
		}
	}
	var newTracks []rekordbox.Track
	var newPlaylists []rekordbox.Playlist

	if hasMDB {
		db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdbPath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()) // read-only: never create -wal/-journal on the user's USB drive
		if err != nil {
			return fmt.Errorf("open engine m.db: %w", err)
		}
		defer db.Close()

		trackRows, err := db.Query(`SELECT id, COALESCE(title, ''), COALESCE(artist, ''), COALESCE(bpm, 0), COALESCE(bpmAnalyzed, 0), COALESCE(length, 0), COALESCE(path, ''), COALESCE(filename, '') FROM Track ORDER BY id`)
		if err != nil {
			return fmt.Errorf("query engine tracks: %w", err)
		}
		defer trackRows.Close()

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
			)
			if err := trackRows.Scan(&id, &title, &artist, &bpmInt, &bpmAnalyzed, &length, &rawPath, &filename); err != nil {
				return fmt.Errorf("scan engine track: %w", err)
			}
			if id == 0 {
				continue
			}
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
			if effectiveBPM <= 0 {
				effectiveBPM = 120.0
			}
			if length <= 0 {
				length = 180
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

			if exist, ok := existingPaths[cleanPath]; ok {
				trackIDMap[id] = exist.ID
				trackMap[exist.ID] = true
				continue
			}

			newID := id
			if newID == 0 || trackMap[newID] {
				maxTrackID++
				newID = maxTrackID
			}
			if newID > maxTrackID {
				maxTrackID = newID
			}
			trackIDMap[id] = newID
			trackMap[newID] = true

			analysisPath, err := writeANLZ(root, newID, cleanPath, effectiveBPM, length)
			if err != nil {
				return err
			}
			fileSize := int64(1048576)
			if st, err := os.Stat(filepath.Join(root, cleanPath)); err == nil && st.Mode().IsRegular() {
				fileSize = st.Size() // CDJs read the file up to the size dbserver reports
			}

			t := rekordbox.Track{
				ID:           newID,
				Path:         cleanPath,
				AnalysisPath: analysisPath,
				Title:        title,
				Artist:       artist,
				BPM:          effectiveBPM,
				Duration:     float64(length),
				SampleRate:   44100,
				BitDepth:     24,
				Bitrate:      320,
				Year:         2026,
				FileSize:     fileSize,
			}
			newTracks = append(newTracks, t)
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
		entityRows, err := db.Query(`SELECT listId, trackId, COALESCE(nextEntityId, 0) FROM PlaylistEntity ORDER BY id`)
		if err == nil {
			type ent struct {
				trackId uint32
				nextId  uint32
			}
			listEnts := make(map[uint32][]ent)
			for entityRows.Next() {
				var listId, trackId, nextId uint32
				if err := entityRows.Scan(&listId, &trackId, &nextId); err == nil {
					if mapped := trackIDMap[trackId]; mapped != 0 && trackMap[mapped] {
						listEnts[listId] = append(listEnts[listId], ent{trackId: mapped, nextId: nextId})
					}
				}
			}
			entityRows.Close()

			for listId, ents := range listEnts {
				var ordered []uint32
				for _, e := range ents {
					ordered = append(ordered, e.trackId)
				}
				entitiesByList[listId] = ordered
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
				continue
			}

			newPID := rp.id
			if newPID == 0 || usedPlaylistIDs[newPID] {
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
				analysisPath, err := writeANLZ(root, id, path, m.BPM, int(m.Duration))
				if err != nil {
					return err
				}
				m.ID, m.Path, m.AnalysisPath = id, path, analysisPath
				newTracks = append(newTracks, m)
			}
			all = append(all, id)
			byPlatform[st.platform] = append(byPlatform[st.platform], id)
		}
		folder := maxPlaylistID + 1
		newPlaylists = append(newPlaylists, rekordbox.Playlist{ID: folder, Name: StreamFolder, Folder: true}, rekordbox.Playlist{ID: folder + 1, ParentID: folder, Name: "All Streaming", Tracks: all})
		maxPlaylistID = folder + 1
		platforms := make([]string, 0, len(byPlatform))
		for p := range byPlatform {
			platforms = append(platforms, p)
		}
		sort.Strings(platforms)
		for _, p := range platforms {
			maxPlaylistID++
			newPlaylists = append(newPlaylists, rekordbox.Playlist{ID: maxPlaylistID, ParentID: folder, Name: p, Tracks: byPlatform[p]})
		}
	}

	if existingLib != nil && len(newTracks) == 0 && len(newPlaylists) == 0 {
		_ = os.WriteFile(stampPath, []byte(currentStamp), 0644)
		return nil
	}

	var mergedTracks []rekordbox.Track
	if existingLib != nil {
		mergedTracks = append(mergedTracks, existingLib.Tracks...)
	}
	mergedTracks = append(mergedTracks, newTracks...)

	var mergedPlaylists []rekordbox.Playlist
	if existingLib != nil {
		mergedPlaylists = append(mergedPlaylists, existingLib.Playlists...)
	}
	mergedPlaylists = append(mergedPlaylists, newPlaylists...)

	for i := range mergedPlaylists {
		mergedPlaylists[i].SortOrder = uint32(i + 1)
	}

	pdbData, err := buildPDB(mergedTracks, mergedPlaylists)
	if err != nil {
		return fmt.Errorf("build export.pdb: %w", err)
	}

	pioneerDir := filepath.Dir(pdbPath)
	if err := os.MkdirAll(pioneerDir, 0755); err != nil {
		return fmt.Errorf("mkdir pioneer dir: %w", err)
	}

	if err := os.WriteFile(pdbPath, pdbData, 0644); err != nil {
		return fmt.Errorf("write export.pdb: %w", err)
	}
	if st, err := os.Stat(pdbPath); err != nil || os.WriteFile(filepath.Join(root, generatedMarker), []byte(pdbStamp(st)), 0644) != nil {
		return fmt.Errorf("mark generated export.pdb: %v", err)
	}

	lib, err := rekordbox.ReadPDB(pdbPath)
	if err != nil {
		return fmt.Errorf("validate generated export.pdb: %w", err)
	}
	if err := lib.Validate(); err != nil {
		return fmt.Errorf("validate library: %w", err)
	}

	_ = os.WriteFile(stampPath, []byte(currentStamp), 0644)
	return nil
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

func pqtzSection(bpm float64, lengthSec int) []byte {
	if bpm <= 0 {
		bpm = 120.0
	}
	if lengthSec <= 0 {
		lengthSec = 180
	}
	beatMS := 60000.0 / bpm
	numBeats := int(float64(lengthSec*1000) / beatMS)
	if numBeats < 4 {
		numBeats = 4
	}
	beats := make([]byte, 12+numBeats*8)
	binary.BigEndian.PutUint32(beats[4:], 0x80000)
	binary.BigEndian.PutUint32(beats[8:], uint32(numBeats))
	bpm100 := uint16(math.Round(bpm * 100))
	for i := 0; i < numBeats; i++ {
		p := beats[12+8*i:]
		binary.BigEndian.PutUint16(p, uint16((i%4)+1))
		binary.BigEndian.PutUint16(p[2:], bpm100)
		binary.BigEndian.PutUint32(p[4:], uint32(math.Round(float64(i)*beatMS)))
	}
	return section("PQTZ", 24, beats)
}

func pwv5Section(samples int) []byte {
	if samples <= 0 {
		samples = 400
	}
	wave := make([]byte, 12+samples*2)
	binary.BigEndian.PutUint32(wave[0:], 2)
	binary.BigEndian.PutUint32(wave[4:], uint32(samples))
	for i := 0; i < samples; i++ {
		binary.BigEndian.PutUint16(wave[12+2*i:], 0xfc7c)
	}
	return section("PWV5", 24, wave)
}

func pwv7Section(samples int) []byte {
	if samples <= 0 {
		samples = 400
	}
	wave := make([]byte, 12+samples*3)
	binary.BigEndian.PutUint32(wave[0:], 3)
	binary.BigEndian.PutUint32(wave[4:], uint32(samples))
	for i := 0; i < samples; i++ {
		wave[12+3*i] = 100
		wave[12+3*i+1] = 80
		wave[12+3*i+2] = 120
	}
	return section("PWV7", 24, wave)
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
	if sampleRate <= 0 {
		sampleRate = 44100
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
			k = "8A"
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
	if len(keyRows) == 0 {
		keyRows = append(keyRows, buildKeyRow(1, "8A"))
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
		kName := t.Key
		if kName == "" {
			kName = "8A"
		}
		kID := keyMap[kName]
		if kID == 0 {
			kID = 1
		}
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
	tables := []tableSpec{
		{0, trackRows},
		{2, artistRows},
		{5, keyRows},
		{7, playlistRows},
		{8, entryRows},
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
func writeANLZ(root string, id uint32, trackPath string, bpm float64, length int) (string, error) {
	anlzRel := fmt.Sprintf("PIONEER/USBANLZ/P%03d", id)
	dir := filepath.Join(root, anlzRel)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("mkdir anlz dir: %w", err)
	}
	for name, body := range map[string][]byte{
		"ANLZ0000.DAT": pmai(ppthSection(trackPath), pqtzSection(bpm, length)),
		"ANLZ0000.EXT": pmai(ppthSection(trackPath), pwv5Section(400)),
		"ANLZ0000.2EX": pmai(ppthSection(trackPath), pwv7Section(400)),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0644); err != nil {
			return "", fmt.Errorf("write anlz %s: %w", name, err)
		}
	}
	return "/" + anlzRel + "/ANLZ0000.DAT", nil
}
