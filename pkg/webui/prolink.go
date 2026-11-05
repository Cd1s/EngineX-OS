package webui

import (
	"encoding/json"
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
	if updated.Pitch != 0 {
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
	if updated.PlayPositionMS != 0 {
		res.PlayPositionMS = updated.PlayPositionMS
	}
	res.Playing = updated.Playing
	res.OnAir = updated.OnAir
	res.Master = updated.Master
	res.Sync = updated.Sync
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
	if env := os.Getenv("SC5000_PROLINK_STATE"); env != "" {
		return env
	}
	return "/tmp/sc5000-prolink.json"
}

func (s *Server) mergedDevices() []prolink.Device {
	devMap := make(map[string]prolink.Device)
	statePath := s.ProlinkStatePath()
	if data, err := os.ReadFile(statePath); err == nil {
		var snap prolink.Snapshot
		if err := json.Unmarshal(data, &snap); err == nil {
			for _, d := range snap.Devices {
				devMap[deviceKey(d)] = d
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
	Enabled   bool                      `json:"enabled"`
	UpdatedAt time.Time                 `json:"updatedAt"`
	Devices   []prolink.Device          `json:"devices"`
	Decks     map[string]prolink.Device `json:"decks"`
}

func (s *Server) prolinkGet(w http.ResponseWriter, r *http.Request) {
	devices := s.mergedDevices()
	snap := prolink.BuildSnapshot(devices)
	resp := prolinkResponse{
		Enabled:   true,
		UpdatedAt: snap.UpdatedAt,
		Devices:   snap.Devices,
		Decks:     snap.Decks,
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
