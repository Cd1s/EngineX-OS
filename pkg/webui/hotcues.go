package webui

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

var (
	hotCuesMu sync.Mutex

	// HotCuePalette defines the 16 standard RGB colors for Denon / Pioneer pads.
	HotCuePalette = []string{
		"#E53935", // Red
		"#FB8C00", // Orange
		"#FDD835", // Yellow
		"#43A047", // Green
		"#00ACC1", // Cyan
		"#1E88E5", // Blue
		"#8E24AA", // Purple
		"#EC407A", // Pink
		"#D32F2F", // Dark Red
		"#F4511E", // Deep Orange
		"#FFB300", // Amber
		"#7CB342", // Light Green
		"#00897B", // Teal
		"#3949AB", // Indigo
		"#5E35B1", // Deep Purple
		"#D81B60", // Rose
	}
)

type HotCueSlot struct {
	Index      int     `json:"index"`
	Label      string  `json:"label"`
	Color      string  `json:"color"`
	PositionMS float64 `json:"positionMs"`
}

type hotCuesResponse struct {
	Drive   string       `json:"drive"`
	TrackID int64        `json:"trackId"`
	Cues    []HotCueSlot `json:"cues"`
	Slots   []HotCueSlot `json:"slots"`
	Palette []string     `json:"palette"`
	// Pending: Engine has the drive's library open, so the edit is written to
	// m.db before Engine next opens it (eject and insert the drive).
	Pending bool `json:"pending,omitempty"`
}

type hotCuePostReq struct {
	Drive      string          `json:"drive"`
	TrackIDRaw json.RawMessage `json:"trackId"`
	Index      int             `json:"index"`
	Label      string          `json:"label"`
	Color      string          `json:"color"`
	PositionMS *float64        `json:"positionMs,omitempty"`
}

func isValidHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		b := s[i]
		if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')) {
			return false
		}
	}
	return true
}

func parseHexColor(c string) (byte, byte, byte, error) {
	if !isValidHexColor(c) {
		return 0, 0, 0, errors.New("invalid hex color")
	}
	v, err := strconv.ParseUint(c[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, err
	}
	return byte((v >> 16) & 0xff), byte((v >> 8) & 0xff), byte(v & 0xff), nil
}

func validateDrive(mediaRoot, drive string) (string, error) {
	if drive == "" || strings.Contains(drive, "/") || strings.Contains(drive, "\\") || strings.Contains(drive, "..") || drive == "." {
		return "", errors.New("invalid drive name")
	}
	driveDir := filepath.Join(mediaRoot, drive)
	rel, err := filepath.Rel(mediaRoot, driveDir)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return "", errors.New("invalid drive path")
	}
	return driveDir, nil
}

func defaultHotCueSlots() []HotCueSlot {
	slots := make([]HotCueSlot, 8)
	for i := 0; i < 8; i++ {
		slots[i] = HotCueSlot{
			Index:      i,
			Label:      "",
			Color:      "",
			PositionMS: 0,
		}
	}
	return slots
}

func readHotCuesFromDB(dbPath string, trackID int64) ([]HotCueSlot, bool) {
	if st, err := os.Stat(dbPath); err != nil || st.Size() == 0 {
		return nil, false
	}
	u := url.URL{Scheme: "file", Path: dbPath}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout(1000)&_pragma=query_only(1)")
	if err != nil {
		return nil, false
	}
	defer db.Close()

	var blob []byte
	err = db.QueryRow("SELECT quickCues FROM PerformanceData WHERE trackId = ?", trackID).Scan(&blob)
	if err != nil || len(blob) == 0 {
		return nil, false
	}

	raw, err := dbhealer.DecodeQCompress(blob)
	if err != nil || len(raw) < 8 || binary.BigEndian.Uint64(raw[:8]) != 8 {
		return nil, false
	}
	// Positions are in samples of the track's own rate (BH-GC2-1: 44100 was assumed).
	rate := enginedb.QuickCueRate(context.Background(), db, trackID)

	slots := defaultHotCueSlots()
	offset := 8
	for i := 0; i < 8; i++ {
		if offset >= len(raw) {
			break
		}
		lLen := int(raw[offset])
		offset++
		if offset+lLen > len(raw) {
			break
		}
		lbl := string(raw[offset : offset+lLen])
		offset += lLen
		if offset+12 > len(raw) {
			break
		}
		pos := math.Float64frombits(binary.BigEndian.Uint64(raw[offset : offset+8]))
		a := raw[offset+8]
		r := raw[offset+9]
		g := raw[offset+10]
		b := raw[offset+11]
		offset += 12

		color := ""
		if a > 0 || r > 0 || g > 0 || b > 0 {
			color = fmt.Sprintf("#%02X%02X%02X", r, g, b)
		}
		posMS := 0.0
		if pos >= 0 {
			posMS = pos * 1000 / rate
		}
		slots[i] = HotCueSlot{
			Index:      i,
			Label:      lbl,
			Color:      color,
			PositionMS: posMS,
		}
	}
	return slots, true
}

// syncHotCueToDB writes the edit into the drive's m.db, or queues it there when
// Engine has the library open (no outside writer while Engine does, BH-GC2-1).
// It reports whether the edit was queued.
func (s *Server) syncHotCueToDB(driveDir string, trackID int64, slotIndex int, label, colorHex string, posMS *float64) bool {
	dbPath := filepath.Join(driveDir, "Engine Library", "Database2", "m.db")
	if st, err := os.Stat(dbPath); err != nil || st.Size() == 0 {
		return false
	}
	if enginedb.LibraryInUse(driveDir) {
		err := enginedb.QueuePending(driveDir, func(p *enginedb.Pending) {
			p.HotCues = append(p.HotCues, enginedb.PendingCue{TrackID: trackID, Index: slotIndex, Label: label, Color: colorHex, PositionMS: posMS})
		})
		return err == nil
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath}).String()+"?_pragma=busy_timeout(1000)")
	if err != nil {
		return false
	}
	defer db.Close()
	_ = enginedb.SetQuickCue(context.Background(), db, trackID, slotIndex, label, colorHex, posMS)
	return false
}

func (s *Server) handleHotCues(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleHotCuesGet(w, r)
	case http.MethodPost:
		s.handleHotCuesPost(w, r)
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleHotCuesGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	drive := q.Get("drive")
	driveDir, err := validateDrive(s.mediaRoot(), drive)
	if err != nil {
		fail(w, 400, err)
		return
	}
	trackIDStr := q.Get("trackId")
	trackID, err := strconv.ParseInt(trackIDStr, 10, 64)
	if err != nil || trackID <= 0 {
		fail(w, 400, errors.New("invalid trackId"))
		return
	}

	hotCuesMu.Lock()
	defer hotCuesMu.Unlock()

	jsonPath := filepath.Join(driveDir, "Engine Library", "Database2", "enginex_hotcues.json")
	var fileMap map[string][]HotCueSlot
	if b, err := os.ReadFile(jsonPath); err == nil {
		_ = json.Unmarshal(b, &fileMap)
	}

	trackKey := strconv.FormatInt(trackID, 10)
	slots, ok := fileMap[trackKey]
	if ok && len(slots) == 8 {
		respond(w, hotCuesResponse{
			Drive:   drive,
			TrackID: trackID,
			Cues:    slots,
			Slots:   slots,
			Palette: HotCuePalette,
		})
		return
	}

	dbPath := filepath.Join(driveDir, "Engine Library", "Database2", "m.db")
	if dbSlots, found := readHotCuesFromDB(dbPath, trackID); found {
		slots = dbSlots
	} else {
		slots = defaultHotCueSlots()
	}

	respond(w, hotCuesResponse{
		Drive:   drive,
		TrackID: trackID,
		Cues:    slots,
		Slots:   slots,
		Palette: HotCuePalette,
	})
}

func (s *Server) handleHotCuesPost(w http.ResponseWriter, r *http.Request) {
	var req hotCuePostReq
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, errors.New("invalid json payload"))
		return
	}

	driveDir, err := validateDrive(s.mediaRoot(), req.Drive)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if st, err := os.Stat(driveDir); err != nil || !st.IsDir() {
		fail(w, 400, errors.New("drive not found"))
		return
	}

	var trackID int64
	if len(req.TrackIDRaw) > 0 && req.TrackIDRaw[0] == '"' {
		var sID string
		if err := json.Unmarshal(req.TrackIDRaw, &sID); err == nil {
			trackID, _ = strconv.ParseInt(sID, 10, 64)
		}
	} else {
		_ = json.Unmarshal(req.TrackIDRaw, &trackID)
	}
	if trackID <= 0 {
		fail(w, 400, errors.New("invalid trackId"))
		return
	}

	if req.Index < 0 || req.Index > 7 {
		fail(w, 400, errors.New("index must be between 0 and 7"))
		return
	}

	if !isValidHexColor(req.Color) {
		fail(w, 400, errors.New("invalid color: must be #RRGGBB"))
		return
	}

	hotCuesMu.Lock()
	defer hotCuesMu.Unlock()

	dbDir := filepath.Join(driveDir, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		fail(w, 500, err)
		return
	}

	jsonPath := filepath.Join(dbDir, "enginex_hotcues.json")
	fileMap := map[string][]HotCueSlot{}
	if b, err := os.ReadFile(jsonPath); err == nil {
		_ = json.Unmarshal(b, &fileMap)
	}

	trackKey := strconv.FormatInt(trackID, 10)
	slots, ok := fileMap[trackKey]
	if !ok || len(slots) != 8 {
		dbPath := filepath.Join(dbDir, "m.db")
		if dbSlots, found := readHotCuesFromDB(dbPath, trackID); found {
			slots = dbSlots
		} else {
			slots = defaultHotCueSlots()
		}
	}

	slots[req.Index].Index = req.Index
	slots[req.Index].Label = req.Label
	slots[req.Index].Color = strings.ToUpper(req.Color)
	if req.PositionMS != nil {
		slots[req.Index].PositionMS = *req.PositionMS
	}

	fileMap[trackKey] = slots

	marshaled, err := json.MarshalIndent(fileMap, "", "  ")
	if err != nil {
		fail(w, 500, err)
		return
	}

	tmp, err := os.CreateTemp(dbDir, ".enginex_hotcues-*")
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(marshaled); err != nil {
		_ = tmp.Close()
		fail(w, 500, err)
		return
	}
	_ = tmp.Sync()
	if err := tmp.Close(); err != nil {
		fail(w, 500, err)
		return
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	if err := os.Rename(tmp.Name(), jsonPath); err != nil {
		fail(w, 500, err)
		return
	}

	pending := s.syncHotCueToDB(driveDir, trackID, req.Index, req.Label, req.Color, req.PositionMS)

	respond(w, hotCuesResponse{
		Drive:   req.Drive,
		TrackID: trackID,
		Cues:    slots,
		Slots:   slots,
		Palette: HotCuePalette,
		Pending: pending,
	})
}
