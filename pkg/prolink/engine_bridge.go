package prolink

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"

	_ "modernc.org/sqlite"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// SyncEngineToPioneerExport checks if <root>/Engine Library/Database2/m.db exists.
// If it does, and no valid Pioneer export.pdb matching m.db's stamp exists,
// it exports or merges tracks, playlists, ANLZ files (DAT, EXT, 2EX) and export.pdb so that
// Pioneer CDJ players can browse and play tracks over Pro DJ Link (dbserver + NFS).
func SyncEngineToPioneerExport(root string) error {
	mdbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	fi, err := os.Stat(mdbPath)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}

	pdbPath := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
	stampPath := filepath.Join(root, "Engine Library", "Database2", ".prolink-exported-stamp")
	currentStamp := fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())

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

	db, err := sql.Open("sqlite", mdbPath)
	if err != nil {
		return fmt.Errorf("open engine m.db: %w", err)
	}
	defer db.Close()

	trackRows, err := db.Query(`SELECT id, COALESCE(title, ''), COALESCE(artist, ''), COALESCE(bpm, 0), COALESCE(bpmAnalyzed, 0), COALESCE(length, 0), COALESCE(path, ''), COALESCE(filename, '') FROM Track ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query engine tracks: %w", err)
	}
	defer trackRows.Close()

	trackIDMap := make(map[uint32]uint32)
	trackMap := make(map[uint32]bool)
	if existingLib != nil {
		for _, t := range existingLib.Tracks {
			trackMap[t.ID] = true
		}
	}
	var newTracks []rekordbox.Track

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

		anlzRel := fmt.Sprintf("PIONEER/USBANLZ/P%03d", newID)
		anlzFullDir := filepath.Join(root, anlzRel)
		if err := os.MkdirAll(anlzFullDir, 0755); err != nil {
			return fmt.Errorf("mkdir anlz dir: %w", err)
		}

		datPath := filepath.Join(anlzFullDir, "ANLZ0000.DAT")
		if _, err := os.Stat(datPath); err != nil {
			datBytes := pmai(ppthSection(cleanPath), pqtzSection(effectiveBPM, length))
			if err := os.WriteFile(datPath, datBytes, 0644); err != nil {
				return fmt.Errorf("write anlz dat: %w", err)
			}
		}

		extPath := filepath.Join(anlzFullDir, "ANLZ0000.EXT")
		if _, err := os.Stat(extPath); err != nil {
			extBytes := pmai(ppthSection(cleanPath), pwv5Section(400))
			if err := os.WriteFile(extPath, extBytes, 0644); err != nil {
				return fmt.Errorf("write anlz ext: %w", err)
			}
		}

		twoExPath := filepath.Join(anlzFullDir, "ANLZ0000.2EX")
		if _, err := os.Stat(twoExPath); err != nil {
			twoExBytes := pmai(ppthSection(cleanPath), pwv7Section(400))
			if err := os.WriteFile(twoExPath, twoExBytes, 0644); err != nil {
				return fmt.Errorf("write anlz 2ex: %w", err)
			}
		}

		analysisPath := "/" + filepath.ToSlash(filepath.Join(anlzRel, "ANLZ0000.DAT"))

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
			FileSize:     1048576,
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
	var newPlaylists []rekordbox.Playlist

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

	pioneerDir := filepath.Join(root, "PIONEER", "rekordbox")
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
