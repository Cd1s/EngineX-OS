package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

var (
	prolinkMu     sync.Mutex
	postedDevices = make(map[*Server]map[string]prolink.Device)
)

func deviceKey(d prolink.Device) string {
	if d.Number != 0 {
		return strconv.Itoa(int(d.Number))
	}
	if d.Address != "" {
		return d.Address
	}
	return d.Name
}

func mergeDevice(old, updated prolink.Device) prolink.Device {
	res := old
	if updated.Address != "" {
		res.Address = updated.Address
	}
	if updated.Name != "" {
		res.Name = updated.Name
	}
	if updated.Number != 0 {
		res.Number = updated.Number
	}
	if updated.PacketType != 0 {
		res.PacketType = updated.PacketType
	}
	if updated.TrackID != 0 {
		res.TrackID = updated.TrackID
	}
	if updated.SourcePlayer != 0 {
		res.SourcePlayer = updated.SourcePlayer
	}
	if updated.SourceSlot != 0 {
		res.SourceSlot = updated.SourceSlot
	}
	if !updated.LastSeen.IsZero() {
		res.LastSeen = updated.LastSeen
	}
	if updated.BPM != 0 {
		res.BPM = updated.BPM
	}
	// 0 is a real pitch (fader centred, TEMPO RESET, synced to the same BPM) in every
	// packet that carries one; only keepalives and unknown packets have none.
	if t := updated.PacketType; updated.Pitch != 0 || t == 0x0a || t == 0x28 || t == 0x0b {
		res.Pitch = updated.Pitch
	}
	if updated.EffectiveBPM != 0 {
		res.EffectiveBPM = updated.EffectiveBPM
	}
	if updated.Beat != 0 {
		res.Beat = updated.Beat
	}
	if updated.BeatInBar != 0 {
		res.BeatInBar = updated.BeatInBar
	}
	if updated.PlayPositionMS != 0 || updated.PacketType == 0x0b && updated.PrecisePosition { // CUE at 0 ms
		res.PlayPositionMS = updated.PlayPositionMS
	}
	if updated.PacketType != 0x0b { // absolute position packets carry no play/master/sync flags
		res.Playing = updated.Playing
		res.PlayState1 = updated.PlayState1 // CUE held is play mode 7, without the play flag (#217)
		res.OnAir = updated.OnAir
		res.Master = updated.Master
		res.Sync = updated.Sync
	}
	if updated.Title != "" {
		res.Title = updated.Title
	}
	if updated.Artist != "" {
		res.Artist = updated.Artist
	}
	if updated.DurationMS != 0 {
		res.DurationMS = updated.DurationMS
	}
	if len(updated.Waveform) > 0 {
		res.Waveform = updated.Waveform
	}
	if len(updated.Beats) > 0 {
		res.Beats = updated.Beats
	}
	return res
}

// ProlinkStatePath returns the location of the Pro DJ Link state JSON file.
func (s *Server) ProlinkStatePath() string {
	if s.cfg.StateDir != "" {
		p := filepath.Join(s.cfg.StateDir, "prolink.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return liveProlinkStatePath()
}

// liveProlinkStatePath is where rb-bridge --prolink keeps the live CDJ/DJM state
// (its --state-file default, SC5000_PROLINK_STATE in its unit).
func liveProlinkStatePath() string {
	if env := os.Getenv("SC5000_PROLINK_STATE"); env != "" {
		return env
	}
	return "/tmp/sc5000-prolink.json"
}

// mergedDevices: rb-bridge's live state, the snapshot of posted devices kept in
// <StateDir>/prolink.json, then the devices posted to this server. The file in
// StateDir used to replace the live state for good once anything was posted,
// across restarts too (BH-GC2-8); now, per device, the newer report wins.
func (s *Server) mergedDevices() []prolink.Device {
	devMap := make(map[string]prolink.Device)
	paths := []string{liveProlinkStatePath()}
	if p := s.ProlinkStatePath(); p != paths[0] {
		paths = append(paths, p)
	}
	for _, statePath := range paths {
		data, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}
		var snap prolink.Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			continue
		}
		for _, d := range snap.Devices {
			k := deviceKey(d)
			if existing, ok := devMap[k]; !ok {
				devMap[k] = d
			} else if d.LastSeen.After(existing.LastSeen) {
				devMap[k] = mergeDevice(existing, d)
			} else {
				devMap[k] = mergeDevice(d, existing)
			}
		}
	}
	prolinkMu.Lock()
	if posted := postedDevices[s]; posted != nil {
		for k, d := range posted {
			if existing, ok := devMap[k]; ok {
				devMap[k] = mergeDevice(existing, d)
			} else {
				devMap[k] = d
			}
		}
	}
	prolinkMu.Unlock()

	var result []prolink.Device
	for _, d := range devMap {
		result = append(result, d)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Number == result[j].Number {
			return result[i].Address < result[j].Address
		}
		return result[i].Number < result[j].Number
	})
	return result
}

func (s *Server) writeProlinkSnapshot(snap prolink.Snapshot) error {
	dir := s.cfg.StateDir
	if dir == "" {
		dir = "/tmp"
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".prolink-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmp.Name(), 0644)
	target := filepath.Join(dir, "prolink.json")
	return os.Rename(tmp.Name(), target)
}

type prolinkResponse struct {
	Enabled     bool                      `json:"enabled"`
	Share       bool                      `json:"share"`  // reads players' media over the LAN (#59)
	Export      bool                      `json:"export"` // experimental export to CDJs (#63)
	ExportState prolink.ExportStatus      `json:"exportState"`
	UpdatedAt   time.Time                 `json:"updatedAt"`
	Devices     []prolink.Device          `json:"devices"`
	Decks       map[string]prolink.Device `json:"decks"`
}

func (s *Server) prolinkWaveformEnabled() bool {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadUI().ProlinkWaveform
}

func (s *Server) prolinkGet(w http.ResponseWriter, r *http.Request) {
	devices := s.mergedDevices()
	snap := prolink.BuildSnapshot(devices)
	s.uiMu.Lock()
	settings := s.loadUI()
	s.uiMu.Unlock()
	resp := prolinkResponse{
		Enabled:   settings.ProlinkWaveform,
		Share:     settings.ProlinkShare,
		Export:    settings.ProlinkExport,
		UpdatedAt: snap.UpdatedAt,
		Devices:   snap.Devices,
		Decks:     snap.Decks,
	}
	if s.prolinkExport != nil {
		resp.ExportState = s.prolinkExport.Status()
	}
	if resp.Devices == nil {
		resp.Devices = []prolink.Device{}
	}
	if resp.Decks == nil {
		resp.Decks = map[string]prolink.Device{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) prolinkPost(w http.ResponseWriter, r *http.Request) {
	// ?share= / ?export= switch #59 / #63; a JSON body posts devices (#51).
	if q := r.URL.Query(); q.Has("share") || q.Has("export") {
		s.prolinkToggle(w, r)
		return
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 2<<20))
	}
	var incoming []prolink.Device
	var envelope struct {
		Devices []prolink.Device `json:"devices"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Devices) > 0 {
		incoming = envelope.Devices
	} else {
		var list []prolink.Device
		if err := json.Unmarshal(body, &list); err == nil && len(list) > 0 {
			incoming = list
		} else {
			var single prolink.Device
			if err := json.Unmarshal(body, &single); err == nil {
				incoming = []prolink.Device{single}
			} else {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
		}
	}

	now := time.Now()
	prolinkMu.Lock()
	if postedDevices[s] == nil {
		postedDevices[s] = make(map[string]prolink.Device)
	}
	for _, dev := range incoming {
		if dev.LastSeen.IsZero() {
			dev.LastSeen = now
		}
		key := deviceKey(dev)
		postedDevices[s][key] = dev
	}
	prolinkMu.Unlock()

	devices := s.mergedDevices()
	snap := prolink.BuildSnapshot(devices)
	_ = s.writeProlinkSnapshot(snap)

	s.prolinkGet(w, r)
}

// prolinkToggle is POST /v1/sc5000/prolink?share=0|1, the same setting as
// /v1/sc5000/ui?prolinkShare=.
func (s *Server) prolinkToggle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !q.Has("share") && !q.Has("export") {
		fail(w, 400, errors.New("invalid setting"))
		return
	}
	for _, key := range []string{"share", "export"} {
		if q.Has(key) && q.Get(key) != "0" && q.Get(key) != "1" {
			fail(w, 400, errors.New("invalid setting"))
			return
		}
	}
	s.uiMu.Lock()
	v := s.loadUI()
	if q.Has("share") {
		v.ProlinkShare = q.Get("share") == "1"
	}
	if q.Has("export") {
		v.ProlinkExport = q.Get("export") == "1"
	}
	err := s.saveUI(v, q.Has("export"))
	s.uiMu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	s.prolinkGet(w, r)
}
