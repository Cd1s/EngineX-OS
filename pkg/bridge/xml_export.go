package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// XMLExportOptions configures the Rekordbox XML exporter.
type XMLExportOptions struct {
	MountPath   string // root directory of the mounted drive, e.g. /media/USB_NAME
	DriveName   string // optional override; defaults to filepath.Base(MountPath)
	TargetOS    string // "mac" (default), "win", "linux"
	DriveLetter string // for win, e.g. "E"
	OutputPath  string // optional custom destination; defaults to <MountPath>/rekordbox.xml
	NoBackup    bool   // skip backing up existing file to .bak before overwriting (defaults to false = backup enabled)
}

// XMLExportSummary contains statistics about the generated Rekordbox XML.
type XMLExportSummary struct {
	Path        string `json:"path"`
	TotalTracks int    `json:"totalTracks"`
	CuesCount   int    `json:"cuesCount"`
	LoopsCount  int    `json:"loopsCount"`
	Playlists   int    `json:"playlists"`
}

// ExportRekordboxXML reads the Engine Library on mountPath, builds a Rekordbox XML
// structure with all Hot Cues and Loops, and writes it to disk.
func ExportRekordboxXML(ctx context.Context, opts XMLExportOptions) (*XMLExportSummary, error) {
	if opts.MountPath == "" {
		return nil, errors.New("mountPath is required")
	}
	dbPath := filepath.Join(opts.MountPath, "Engine Library", "Database2", "m.db")
	if st, err := os.Stat(dbPath); err != nil || st.Size() == 0 {
		return nil, fmt.Errorf("no Engine Library found on %s", opts.MountPath)
	}

	xmlDB, summary, err := BuildRekordboxXML(ctx, opts)
	if err != nil {
		return nil, err
	}

	outPath := opts.OutputPath
	if outPath == "" {
		outPath = filepath.Join(opts.MountPath, "rekordbox.xml")
	}

	var buf bytes.Buffer
	if _, err := xmlDB.WriteTo(&buf); err != nil {
		return nil, fmt.Errorf("serialize xml: %w", err)
	}

	if !opts.NoBackup {
		if _, err := os.Stat(outPath); err == nil {
			_ = os.Rename(outPath, outPath+".bak")
		}
	}

	tmpFile := outPath + ".tmp"
	if err := os.WriteFile(tmpFile, buf.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("write xml: %w", err)
	}
	_ = os.Remove(outPath) // safety for FAT32/exFAT overwrite
	if err := os.Rename(tmpFile, outPath); err != nil {
		_ = os.Remove(tmpFile)
		return nil, fmt.Errorf("commit xml: %w", err)
	}

	summary.Path = outPath
	return summary, nil
}

// BuildRekordboxXML constructs the Rekordbox XML data structure from an Engine Library.
func BuildRekordboxXML(ctx context.Context, opts XMLExportOptions) (*rekordbox.XMLDatabase, *XMLExportSummary, error) {
	mountPath := filepath.Clean(opts.MountPath)
	dbPath := filepath.Join(mountPath, "Engine Library", "Database2", "m.db")
	u := url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, nil, fmt.Errorf("open engine db: %w", err)
	}
	defer db.Close()

	driveName := opts.DriveName
	if driveName == "" {
		driveName = filepath.Base(mountPath)
	}
	targetOS := strings.ToLower(opts.TargetOS)
	if targetOS == "" {
		targetOS = "mac"
	}
	driveLetter := strings.ToUpper(opts.DriveLetter)
	if driveLetter == "" {
		driveLetter = "E"
	}
	driveLetter = strings.TrimSuffix(driveLetter, ":")

	// Read pending hotcue overrides from WebUI if available
	jsonPath := filepath.Join(mountPath, "Engine Library", "Database2", "enginex_hotcues.json")
	var fileMap map[string][]struct {
		Index      int     `json:"index"`
		Label      string  `json:"label"`
		Color      string  `json:"color"`
		PositionMS float64 `json:"positionMs"`
	}
	if b, err := os.ReadFile(jsonPath); err == nil {
		_ = json.Unmarshal(b, &fileMap)
	}

	xmlDB := rekordbox.NewXMLDatabase()
	summary := &XMLExportSummary{}

	// Map engine track ID -> exported Rekordbox TrackID string
	engineToExportID := make(map[int64]string)

	rows, err := db.QueryContext(ctx, `
		SELECT
			t.id,
			t.pdbImportKey,
			COALESCE(t.title, ''),
			COALESCE(t.artist, ''),
			COALESCE(t.album, ''),
			COALESCE(t.genre, ''),
			COALESCE(t.comment, ''),
			COALESCE(t.composer, ''),
			COALESCE(t.remixer, ''),
			COALESCE(t.label, ''),
			COALESCE(t.year, 0),
			COALESCE(t.key, -1),
			COALESCE(t.rating, 0),
			COALESCE(t.bpmAnalyzed, 0.0),
			COALESCE(t.bpm, 0),
			COALESCE(t.length, 0),
			COALESCE(t.bitrate, 0),
			COALESCE(t.fileBytes, 0),
			COALESCE(t.filename, ''),
			COALESCE(t.path, ''),
			COALESCE(t.fileType, ''),
			p.trackData,
			p.quickCues,
			p.loops,
			p.beatData
		FROM Track t
		LEFT JOIN PerformanceData p ON p.trackId = t.id
		ORDER BY t.id
	`)
	if err != nil {
		return nil, nil, fmt.Errorf("query tracks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, fileBytes                                         int64
			pdbImportKey                                          sql.NullInt64
			title, artist, album, genre, comment, composer        string
			remixer, label, filename, tPath, fileType             string
			year, key, rating, bpm, length, bitrate               int
			bpmAnalyzed                                           float64
			trackDataBlob, quickCuesBlob, loopsBlob, beatDataBlob []byte
		)
		if err := rows.Scan(
			&id, &pdbImportKey, &title, &artist, &album, &genre, &comment, &composer,
			&remixer, &label, &year, &key, &rating, &bpmAnalyzed, &bpm, &length,
			&bitrate, &fileBytes, &filename, &tPath, &fileType,
			&trackDataBlob, &quickCuesBlob, &loopsBlob, &beatDataBlob,
		); err != nil {
			return nil, nil, fmt.Errorf("scan track: %w", err)
		}

		exportID := strconv.FormatInt(id, 10)
		if pdbImportKey.Valid && pdbImportKey.Int64 > 0 {
			exportID = strconv.FormatInt(pdbImportKey.Int64, 10)
		}
		engineToExportID[id] = exportID

		relPath := resolveTrackRelPath(tPath, mountPath)
		var osPath string
		switch targetOS {
		case "win", "windows":
			osPath = driveLetter + ":/" + relPath
		case "linux":
			osPath = filepath.ToSlash(filepath.Join(mountPath, relPath))
		default: // "mac"
			osPath = "/Volumes/" + driveName + "/" + relPath
		}
		location := rekordbox.EncodeXMLLocation(osPath)

		if title == "" {
			title = filename
			if title == "" {
				title = filepath.Base(relPath)
			}
		}

		kind := deriveAudioKind(fileType, filename, relPath)

		var sampleRate float64 = 44100
		var samples float64 = 0
		if len(trackDataBlob) > 0 {
			if raw, err := enginedb.QUncompress(trackDataBlob, 4096); err == nil && len(raw) >= 16 {
				sr := math.Float64frombits(binary.BigEndian.Uint64(raw[:8]))
				if sr >= 8000 && sr <= 384000 {
					sampleRate = sr
				}
				sCount := binary.BigEndian.Uint64(raw[8:16])
				if sCount > 0 {
					samples = float64(sCount)
				}
			}
		}

		var avgBPM string
		if bpmAnalyzed > 0 {
			avgBPM = fmt.Sprintf("%.2f", bpmAnalyzed)
		} else if bpm > 0 {
			avgBPM = fmt.Sprintf("%.2f", float64(bpm)/100.0)
		}

		var tonality string
		if key >= 0 && key <= 23 {
			k := analyzer.KeyFromEngineID(key)
			tonality = k.Name
		}

		xmlTrack := rekordbox.XMLTrack{
			TrackID:     exportID,
			Name:        title,
			Artist:      artist,
			Composer:    composer,
			Album:       album,
			Genre:       genre,
			Kind:        kind,
			Size:        fileBytes,
			TotalTime:   length,
			DiscNumber:  0,
			TrackNumber: 0,
			Year:        year,
			AverageBpm:  avgBPM,
			BitRate:     bitrate,
			SampleRate:  fmt.Sprintf("%.0f", sampleRate),
			Comments:    comment,
			PlayCount:   0,
			Rating:      mapEngineRating(rating),
			Location:    location,
			Remixer:     remixer,
			Tonality:    tonality,
			Label:       label,
		}

		if avgBPM != "" && avgBPM != "0.00" {
			xmlTrack.Tempos = []rekordbox.XMLTempo{
				{Inizio: "0.000", Bpm: avgBPM, Metro: "4/4", Battito: 1},
			}
		}

		var posMarks []rekordbox.XMLPositionMark
		usedPads := make(map[int]bool)

		// 1. Hot cues from database or JSON overrides
		trackKey := strconv.FormatInt(id, 10)
		if fileSlots, ok := fileMap[trackKey]; ok && len(fileSlots) == 8 {
			for _, slot := range fileSlots {
				if slot.PositionMS >= 0 && slot.Index >= 0 && slot.Index <= 7 {
					r, g, b := rekordbox.ParseHexRGB(slot.Color)
					posMarks = append(posMarks, rekordbox.XMLPositionMark{
						Name:  slot.Label,
						Type:  "0",
						Start: fmt.Sprintf("%.3f", slot.PositionMS/1000.0),
						Num:   strconv.Itoa(slot.Index),
						Red:   r,
						Green: g,
						Blue:  b,
					})
					usedPads[slot.Index] = true
					summary.CuesCount++
				}
			}
			// When fileMap overrides Hot Cues 0-7, still preserve Main Cue from quickCuesBlob if present
			if len(quickCuesBlob) > 0 {
				if cues, err := enginedb.ParseDisplayCues(quickCuesBlob, sampleRate, samples); err == nil {
					for _, c := range cues {
						if c.I == 0 {
							posMarks = append(posMarks, rekordbox.XMLPositionMark{
								Name:  c.N,
								Type:  "0",
								Start: fmt.Sprintf("%.3f", c.MS/1000.0),
								Num:   "-1",
							})
							summary.CuesCount++
							break
						}
					}
				}
			}
		} else if len(quickCuesBlob) > 0 {
			if cues, err := enginedb.ParseDisplayCues(quickCuesBlob, sampleRate, samples); err == nil {
				for _, c := range cues {
					if c.I >= 1 && c.I <= 8 {
						padIdx := c.I - 1
						r, g, b := rekordbox.ParseHexRGB(c.C)
						posMarks = append(posMarks, rekordbox.XMLPositionMark{
							Name:  c.N,
							Type:  "0",
							Start: fmt.Sprintf("%.3f", c.MS/1000.0),
							Num:   strconv.Itoa(padIdx),
							Red:   r,
							Green: g,
							Blue:  b,
						})
						usedPads[padIdx] = true
						summary.CuesCount++
					} else if c.I == 0 { // Main memory cue
						posMarks = append(posMarks, rekordbox.XMLPositionMark{
							Name:  c.N,
							Type:  "0",
							Start: fmt.Sprintf("%.3f", c.MS/1000.0),
							Num:   "-1",
						})
						summary.CuesCount++
					}
				}
			}
		}

		// 2. Loops from database
		if len(loopsBlob) > 0 {
			if loops, err := enginedb.ParseDisplayLoops(loopsBlob, sampleRate, samples); err == nil {
				for _, l := range loops {
					r, g, b := rekordbox.ParseHexRGB(l.C)
					// In Rekordbox XML, Type="4" (Loop) with Num="0".."7" is a Hot Loop sharing pads A-H with Hot Cues.
					// If the pad slot is already taken by a Hot Cue, export as Memory Loop (Num="-1") to avoid collision.
					numStr := "-1"
					if l.Slot >= 0 && l.Slot <= 7 && !usedPads[l.Slot] {
						numStr = strconv.Itoa(l.Slot)
						usedPads[l.Slot] = true
					}
					posMarks = append(posMarks, rekordbox.XMLPositionMark{
						Name:  l.N,
						Type:  "4",
						Start: fmt.Sprintf("%.3f", l.StartMS/1000.0),
						End:   fmt.Sprintf("%.3f", l.EndMS/1000.0),
						Num:   numStr,
						Red:   r,
						Green: g,
						Blue:  b,
					})
					summary.LoopsCount++
				}
			}
		}

		xmlTrack.PositionMarks = posMarks
		xmlDB.Collection.Tracks = append(xmlDB.Collection.Tracks, xmlTrack)
		summary.TotalTracks++
	}
	xmlDB.Collection.Entries = len(xmlDB.Collection.Tracks)

	// Build Playlists
	exportPlaylists(ctx, db, xmlDB, engineToExportID, summary)

	return xmlDB, summary, nil
}

func resolveTrackRelPath(tPath, mountPath string) string {
	clean := filepath.Clean(tPath)
	if filepath.IsAbs(clean) {
		if rel, err := filepath.Rel(mountPath, clean); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		return filepath.ToSlash(filepath.Base(clean))
	}
	// Join with Engine Library
	abs := filepath.Clean(filepath.Join(mountPath, "Engine Library", clean))
	if rel, err := filepath.Rel(mountPath, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	rel := filepath.ToSlash(clean)
	for strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		rel = strings.TrimPrefix(rel, "../")
		rel = strings.TrimPrefix(rel, "/")
	}
	return rel
}

func deriveAudioKind(fileType, filename, relPath string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = strings.ToLower(filepath.Ext(relPath))
	}
	switch ext {
	case ".mp3":
		return "MP3 File"
	case ".wav":
		return "WAV File"
	case ".flac":
		return "FLAC File"
	case ".aif", ".aiff":
		return "AIFF File"
	case ".m4a", ".aac":
		return "AAC File"
	case ".ogg":
		return "OGG File"
	case ".opus":
		return "Opus File"
	}
	if fileType != "" {
		return strings.ToUpper(fileType) + " File"
	}
	return "Audio File"
}

func mapEngineRating(r int) int {
	switch r {
	case 1:
		return 51
	case 2:
		return 102
	case 3:
		return 153
	case 4:
		return 204
	case 5:
		return 255
	default:
		return 0
	}
}

type playlistRow struct {
	id       int64
	title    string
	parentID sql.NullInt64
}

func exportPlaylists(ctx context.Context, db *sql.DB, xmlDB *rekordbox.XMLDatabase, engineToExportID map[int64]string, summary *XMLExportSummary) {
	rows, err := db.QueryContext(ctx, `SELECT id, title, parentListId FROM Playlist ORDER BY id`)
	if err != nil {
		return
	}
	defer rows.Close()

	var allLists []playlistRow
	for rows.Next() {
		var r playlistRow
		if err := rows.Scan(&r.id, &r.title, &r.parentID); err == nil {
			allLists = append(allLists, r)
		}
	}

	if len(allLists) == 0 {
		return
	}

	// Group by parentListId (0 or null means top level)
	childrenOf := make(map[int64][]playlistRow)
	for _, l := range allLists {
		var pid int64 = 0
		if l.parentID.Valid {
			pid = l.parentID.Int64
		}
		childrenOf[pid] = append(childrenOf[pid], l)
	}

	// Fetch track membership for all playlists
	listTracks := make(map[int64][]string)
	eRows, err := db.QueryContext(ctx, `SELECT listId, trackId FROM PlaylistEntity ORDER BY id`)
	if err == nil {
		defer eRows.Close()
		for eRows.Next() {
			var lID, tID int64
			if err := eRows.Scan(&lID, &tID); err == nil {
				if expID, ok := engineToExportID[tID]; ok {
					listTracks[lID] = append(listTracks[lID], expID)
				}
			}
		}
	}

	visited := make(map[int64]bool)
	var buildNodes func(parentID int64, depth int) []rekordbox.XMLNode
	buildNodes = func(parentID int64, depth int) []rekordbox.XMLNode {
		if depth > 32 {
			return nil
		}
		var nodes []rekordbox.XMLNode
		for _, pl := range childrenOf[parentID] {
			if visited[pl.id] {
				continue
			}
			visited[pl.id] = true
			subChildren := childrenOf[pl.id]
			tracks := listTracks[pl.id]
			if len(subChildren) > 0 {
				// It's a folder
				subNodes := buildNodes(pl.id, depth+1)
				nodes = append(nodes, rekordbox.XMLNode{
					Type:  "0",
					Name:  pl.title,
					Count: len(subNodes),
					Nodes: subNodes,
				})
			} else {
				// It's a playlist
				var nodeTracks []rekordbox.XMLNodeTrack
				for _, trKey := range tracks {
					nodeTracks = append(nodeTracks, rekordbox.XMLNodeTrack{Key: trKey})
				}
				nodes = append(nodes, rekordbox.XMLNode{
					Type:    "1",
					Name:    pl.title,
					KeyType: "0",
					Entries: len(nodeTracks),
					Tracks:  nodeTracks,
				})
				summary.Playlists++
			}
		}
		return nodes
	}

	rootNodes := buildNodes(0, 0)
	xmlDB.Playlists.Root.Nodes = rootNodes
	xmlDB.Playlists.Root.Count = len(rootNodes)
}
