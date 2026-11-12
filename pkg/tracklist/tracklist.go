// Package tracklist records, merges, persists, and exports timestamped DJ set
// tracklists for both all-in-one consoles and linked standalone players (#199).
package tracklist

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	DefaultAudibleHold  = 1500 * time.Millisecond
	DefaultFallbackHold = 10 * time.Second
	dedupWindowSec      = 25.0
	maxSavedSessions    = 50
)

// Entry represents a single track played on air during a DJ set.
type Entry struct {
	OffsetSec   float64   `json:"offsetSec"`
	StartedAt   time.Time `json:"startedAt"`
	Player      int       `json:"player,omitempty"` // 1..4 on standalone players; 0 on all-in-ones
	Deck        int       `json:"deck"`             // 1..4
	Channel     int       `json:"channel,omitempty"`
	SourceLabel string    `json:"sourceLabel,omitempty"`
	Artist      string    `json:"artist"`
	Title       string    `json:"title"`
	BPM         float64   `json:"bpm,omitempty"`
	Key         string    `json:"key,omitempty"`
	Streaming   bool      `json:"streaming,omitempty"`
}

// MixcloudSection represents a single section in Mixcloud's upload schema.
type MixcloudSection struct {
	Index        int    `json:"index"`
	StartTimeSec int    `json:"startTime"`
	Artist       string `json:"artist"`
	Song         string `json:"song"`
}

// Session represents an active or completed DJ set tracklist.
type Session struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Mode            string    `json:"mode"` // "stream_and_tracklist", "recorder", or "tracklist_only"
	Active          bool      `json:"active"`
	StartedAt       time.Time `json:"startedAt"`
	EndedAt         time.Time `json:"endedAt,omitempty"`
	AudioFile       string    `json:"audioFile,omitempty"`
	StreamingNotice bool      `json:"streamingNotice,omitempty"`
	Entries         []Entry   `json:"entries"`
	Revision        int64     `json:"revision"`
}

// DeckCandidate describes the live state of one physical deck (never a shadow slot).
type DeckCandidate struct {
	Player      int
	Deck        int
	Channel     int
	SourceLabel string
	Artist      string
	Title       string
	BPM         float64
	Key         string
	Playing     bool // true playback only (Cue preview excluded)
	OnAir       bool // mixer fader + crossfader audible when MixerKnown is true
	MixerKnown  bool // true: console internal mixer, StageLinQ X1800/X1850, or Pioneer DJM
	Streaming   bool
}

type deckWatch struct {
	trackKey     string
	audibleSince time.Time
	committedKey string
}

// PeerSyncMessage replicates session lifecycle and entries between linked EngineX players.
type PeerSyncMessage struct {
	Action  string  `json:"action"` // "sync" or "stop"
	Session Session `json:"session"`
}

// Config configures a Manager.
type Config struct {
	StateDir     string
	AudibleHold  time.Duration
	FallbackHold time.Duration
}

// Manager tracks the active set session and persists completed tracklists.
type Manager struct {
	mu           sync.Mutex
	stateDir     string
	audibleHold  time.Duration
	fallbackHold time.Duration
	current      *Session
	history      []Session
	watches      map[string]*deckWatch
}

func NewManager(cfg Config) *Manager {
	ah := cmp.Or(cfg.AudibleHold, DefaultAudibleHold)
	fh := cmp.Or(cfg.FallbackHold, DefaultFallbackHold)
	m := &Manager{
		stateDir:     cfg.StateDir,
		audibleHold:  ah,
		fallbackHold: fh,
		watches:      map[string]*deckWatch{},
	}
	m.loadHistoryLocked()
	return m
}

func (m *Manager) SetFallbackHold(d time.Duration) {
	m.mu.Lock()
	if d >= time.Second && d <= 5*time.Minute {
		m.fallbackHold = d
	}
	m.mu.Unlock()
}

func (m *Manager) FallbackHold() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fallbackHold
}

func (m *Manager) tracklistsDir() string {
	if m.stateDir == "" {
		return ""
	}
	return filepath.Join(m.stateDir, "tracklists")
}

func (m *Manager) loadHistoryLocked() {
	dir := m.tracklistsDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var loaded []Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(raw, &s) == nil && s.ID != "" {
			s.Active = false
			loaded = append(loaded, s)
		}
	}
	slices.SortFunc(loaded, func(a, b Session) int {
		return b.StartedAt.Compare(a.StartedAt)
	})
	if len(loaded) > maxSavedSessions {
		loaded = loaded[:maxSavedSessions]
	}
	m.history = loaded
}

func cleanField(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func normSongKey(artist, title string) string {
	return strings.ToLower(cleanField(artist)) + "\x00" + strings.ToLower(cleanField(title))
}

// Start opens a new tracklist session at now (or returns the already-active session).
func (m *Manager) Start(now time.Time, title, mode, audioFile string) Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.Active {
		return cloneSession(*m.current)
	}
	if now.IsZero() {
		now = time.Now()
	}
	title = cleanField(title)
	if title == "" {
		title = "EngineX Live Set " + now.UTC().Format("2006-01-02 15:04")
	}
	if mode == "" {
		mode = "tracklist_only"
	}
	id := fmt.Sprintf("set-%s-%04d", now.UTC().Format("20060102-150405"), now.Nanosecond()/1e6)
	m.current = &Session{
		ID:        id,
		Title:     title,
		Mode:      mode,
		Active:    true,
		StartedAt: now,
		AudioFile: cleanField(audioFile),
		Entries:   []Entry{},
		Revision:  1,
	}
	clear(m.watches)
	return cloneSession(*m.current)
}

// Stop closes the active session, writes its JSON + sidecars to stateDir/tracklists,
// and returns the finalized Session.
func (m *Manager) Stop(now time.Time) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || !m.current.Active {
		if len(m.history) > 0 {
			return cloneSession(m.history[0]), false
		}
		return Session{}, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	m.current.Active = false
	m.current.EndedAt = now
	m.current.Revision++
	done := cloneSession(*m.current)
	m.current = nil
	clear(m.watches)
	m.prependHistoryLocked(done)
	m.persistSessionLocked(done)
	return done, true
}

func (m *Manager) prependHistoryLocked(done Session) {
	m.history = slices.DeleteFunc(m.history, func(s Session) bool { return s.ID == done.ID })
	m.history = append([]Session{done}, m.history...)
	if len(m.history) > maxSavedSessions {
		m.history = m.history[:maxSavedSessions]
	}
}

func (m *Manager) persistSessionLocked(s Session) {
	dir := m.tracklistsDir()
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o700)
	_ = WriteSidecars(s, dir, s.ID, s.AudioFile)
}

// Active reports the current active session, if any.
func (m *Manager) Active() (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.Active {
		return cloneSession(*m.current), true
	}
	return Session{}, false
}

// Latest returns the active session if one is running, else the most recent completed session.
func (m *Manager) Latest() (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.Active {
		return cloneSession(*m.current), true
	}
	if len(m.history) > 0 {
		return cloneSession(m.history[0]), true
	}
	return Session{}, false
}

// SessionByID looks up the active session or a saved session by ID ("" means Latest).
func (m *Manager) SessionByID(id string) (Session, bool) {
	if id == "" {
		return m.Latest()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.ID == id {
		return cloneSession(*m.current), true
	}
	for _, s := range m.history {
		if s.ID == id {
			return cloneSession(s), true
		}
	}
	return Session{}, false
}

// Sessions lists recent completed sessions (newest first), plus the active session first if running.
func (m *Manager) Sessions() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	if m.current != nil && m.current.Active {
		out = append(out, cloneSession(*m.current))
	}
	for _, s := range m.history {
		out = append(out, cloneSession(s))
	}
	return out
}

// ObserveCandidates evaluates all physical decks at timestamp now and appends any
// newly qualified audible tracks to the active session. Returnsadded entries and updated session.
func (m *Manager) ObserveCandidates(now time.Time, cands []DeckCandidate) ([]Entry, Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || !m.current.Active {
		return nil, Session{}, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	var added []Entry
	seenWatches := map[string]bool{}
	for _, c := range cands {
		artist, title := cleanField(c.Artist), cleanField(c.Title)
		wkey := fmt.Sprintf("p%d-d%d", c.Player, c.Deck)
		seenWatches[wkey] = true
		w := m.watches[wkey]
		if w == nil {
			w = &deckWatch{}
			m.watches[wkey] = w
		}
		tkey := normSongKey(artist, title)
		audible := c.Playing && (artist != "" || title != "") && (!c.MixerKnown || c.OnAir)
		if !audible {
			w.audibleSince = time.Time{}
			if !c.Playing {
				w.committedKey = ""
			}
			continue
		}
		if w.trackKey != tkey {
			w.trackKey = tkey
			w.audibleSince = now
			w.committedKey = ""
		} else if w.audibleSince.IsZero() {
			w.audibleSince = now
		}
		if w.committedKey == tkey {
			continue
		}
		hold := m.fallbackHold
		if c.MixerKnown {
			hold = m.audibleHold
		}
		if now.Sub(w.audibleSince) < hold {
			continue
		}
		onset := w.audibleSince
		offset := onset.Sub(m.current.StartedAt).Seconds()
		if offset < 0 {
			offset = 0
			onset = m.current.StartedAt
		}
		offset = math.Round(offset*10) / 10
		entry := Entry{
			OffsetSec:   offset,
			StartedAt:   onset.UTC(),
			Player:      c.Player,
			Deck:        c.Deck,
			Channel:     c.Channel,
			SourceLabel: cleanField(c.SourceLabel),
			Artist:      artist,
			Title:       title,
			BPM:         math.Round(c.BPM*100) / 100,
			Key:         cleanField(c.Key),
			Streaming:   c.Streaming,
		}
		w.committedKey = tkey
		if m.addEntryLocked(entry) {
			added = append(added, entry)
		}
	}
	for k := range m.watches {
		if !seenWatches[k] {
			delete(m.watches, k)
		}
	}
	if len(added) > 0 {
		m.persistSessionLocked(*m.current)
	}
	return added, cloneSession(*m.current), len(added) > 0
}

// RecordDirectTrack appends a track reported directly (e.g. QML console mixer audibility)
// at now (with optional onsetBackdate).
func (m *Manager) RecordDirectTrack(now time.Time, e Entry) (Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || !m.current.Active {
		return Entry{}, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	e.Artist = cleanField(e.Artist)
	e.Title = cleanField(e.Title)
	e.SourceLabel = cleanField(e.SourceLabel)
	e.Key = cleanField(e.Key)
	if e.Artist == "" && e.Title == "" {
		return Entry{}, false
	}
	if e.StartedAt.IsZero() {
		e.StartedAt = now.UTC()
	}
	off := e.StartedAt.Sub(m.current.StartedAt).Seconds()
	if off < 0 {
		off = 0
		e.StartedAt = m.current.StartedAt
	}
	e.OffsetSec = math.Round(off*10) / 10
	if !m.addEntryLocked(e) {
		return Entry{}, false
	}
	m.persistSessionLocked(*m.current)
	return e, true
}

func (m *Manager) addEntryLocked(e Entry) bool {
	if m.current == nil {
		return false
	}
	key := normSongKey(e.Artist, e.Title)
	// Suppress duplicate if the immediately preceding track is identical or if the
	// same song was logged within dedupWindowSec (e.g. both players reporting or quick fader cut).
	n := len(m.current.Entries)
	if n > 0 && normSongKey(m.current.Entries[n-1].Artist, m.current.Entries[n-1].Title) == key {
		return false
	}
	for i := n - 1; i >= 0; i-- {
		prev := m.current.Entries[i]
		if math.Abs(prev.OffsetSec-e.OffsetSec) <= dedupWindowSec && normSongKey(prev.Artist, prev.Title) == key {
			if e.OffsetSec < prev.OffsetSec {
				m.current.Entries[i].OffsetSec = e.OffsetSec
				m.current.Entries[i].StartedAt = e.StartedAt
				m.sortEntriesLocked()
				m.current.Revision++
			}
			return false
		}
	}
	if e.Streaming {
		m.current.StreamingNotice = true
	}
	m.current.Entries = append(m.current.Entries, e)
	m.sortEntriesLocked()
	m.current.Revision++
	return true
}

func (m *Manager) sortEntriesLocked() {
	slices.SortStableFunc(m.current.Entries, func(a, b Entry) int {
		if c := cmp.Compare(a.OffsetSec, b.OffsetSec); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Player, b.Player); c != 0 {
			return c
		}
		return cmp.Compare(a.Deck, b.Deck)
	})
}

// ApplyPeerSync merges a peer player's tracklist session state into this manager.
func (m *Manager) ApplyPeerSync(msg PeerSyncMessage) Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := msg.Session
	if p.ID == "" {
		if m.current != nil {
			return cloneSession(*m.current)
		}
		return Session{}
	}
	if msg.Action == "stop" || !p.Active {
		if m.current != nil && m.current.ID == p.ID {
			for _, e := range p.Entries {
				m.addEntryLocked(e)
			}
			m.current.Active = false
			if !p.EndedAt.IsZero() {
				m.current.EndedAt = p.EndedAt
			} else {
				m.current.EndedAt = time.Now().UTC()
			}
			m.current.Revision++
			done := cloneSession(*m.current)
			m.current = nil
			clear(m.watches)
			m.prependHistoryLocked(done)
			m.persistSessionLocked(done)
			return done
		}
		p.Active = false
		m.prependHistoryLocked(cloneSession(p))
		m.persistSessionLocked(p)
		return cloneSession(p)
	}

	// Active peer session: adopt if we have none (or if peer's session started earlier)
	if m.current == nil || !m.current.Active {
		cp := cloneSession(p)
		cp.Entries = nil
		m.current = &cp
		clear(m.watches)
	} else if m.current.ID != p.ID && p.StartedAt.Before(m.current.StartedAt) {
		oldEntries := m.current.Entries
		cp := cloneSession(p)
		cp.Entries = nil
		m.current = &cp
		for _, oe := range oldEntries {
			oe.OffsetSec = math.Max(0, math.Round(oe.StartedAt.Sub(m.current.StartedAt).Seconds()*10)/10)
			m.addEntryLocked(oe)
		}
	}
	if m.current.ID == p.ID {
		for _, e := range p.Entries {
			e.OffsetSec = math.Max(0, math.Round(e.StartedAt.Sub(m.current.StartedAt).Seconds()*10)/10)
			m.addEntryLocked(e)
		}
		m.persistSessionLocked(*m.current)
	}
	return cloneSession(*m.current)
}

func cloneSession(s Session) Session {
	s.Entries = slices.Clone(s.Entries)
	return s
}

func formatHMS(sec float64) string {
	total := max(0, int(math.Round(sec)))
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func displayArtistTitle(artist, title string) string {
	artist, title = cleanField(artist), cleanField(title)
	switch {
	case artist != "" && title != "":
		return artist + " - " + title
	case title != "":
		return title
	default:
		return artist
	}
}

// NowPlayingLine returns the latest track as "Artist - Title" for OBS text overlays.
func NowPlayingLine(s Session) string {
	if len(s.Entries) == 0 {
		return ""
	}
	last := s.Entries[len(s.Entries)-1]
	return displayArtistTitle(last.Artist, last.Title)
}

// FormatText formats s as a human-readable plain-text set tracklist.
func FormatText(s Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", cmp.Or(cleanField(s.Title), "EngineX Set Tracklist"))
	if !s.StartedAt.IsZero() {
		fmt.Fprintf(&b, "# Started: %s\n", s.StartedAt.UTC().Format(time.RFC3339))
	}
	for i, e := range s.Entries {
		line := fmt.Sprintf("%02d. %s  %s", i+1, formatHMS(e.OffsetSec), displayArtistTitle(e.Artist, e.Title))
		var meta []string
		if e.SourceLabel != "" {
			meta = append(meta, e.SourceLabel)
		} else if e.Player > 0 {
			meta = append(meta, fmt.Sprintf("Player %d Deck %d", e.Player, e.Deck))
		} else if e.Deck > 0 {
			meta = append(meta, fmt.Sprintf("Deck %d", e.Deck))
		}
		if e.BPM > 0 {
			meta = append(meta, fmt.Sprintf("%.1f BPM", e.BPM))
		}
		if e.Key != "" && e.Key != "-" {
			meta = append(meta, e.Key)
		}
		if len(meta) > 0 {
			line += "  [" + strings.Join(meta, " · ") + "]"
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func cueEscape(s string) string {
	return strings.ReplaceAll(cleanField(s), `"`, `'`)
}

// FormatCUE formats s as a standard CUE sheet (75 frames/sec INDEX 01 MM:SS:FF).
func FormatCUE(s Session, audioFile string) string {
	var b strings.Builder
	b.WriteString("REM GENERATOR \"EngineX OS\"\n")
	if !s.StartedAt.IsZero() {
		fmt.Fprintf(&b, "REM DATE \"%s\"\n", s.StartedAt.UTC().Format("2006-01-02"))
	}
	fmt.Fprintf(&b, "TITLE \"%s\"\n", cueEscape(cmp.Or(s.Title, "EngineX Live Set")))
	fname := cueEscape(cmp.Or(audioFile, s.AudioFile, cmp.Or(s.ID, "live-set")+".mp3"))
	ftype := "MP3"
	if strings.HasSuffix(strings.ToLower(fname), ".wav") {
		ftype = "WAVE"
	} else if strings.HasSuffix(strings.ToLower(fname), ".flac") {
		ftype = "FLAC"
	}
	fmt.Fprintf(&b, "FILE \"%s\" %s\n", filepath.Base(fname), ftype)
	for i, e := range s.Entries {
		tn := min(i+1, 99)
		sec := math.Max(0, e.OffsetSec)
		whole := int(math.Floor(sec))
		frames := int(math.Round((sec - float64(whole)) * 75.0))
		if frames >= 75 {
			whole++
			frames = 0
		}
		mins := whole / 60
		remSec := whole % 60
		fmt.Fprintf(&b, "  TRACK %02d AUDIO\n", tn)
		fmt.Fprintf(&b, "    TITLE \"%s\"\n", cueEscape(cmp.Or(e.Title, "Untitled")))
		if e.Artist != "" {
			fmt.Fprintf(&b, "    PERFORMER \"%s\"\n", cueEscape(e.Artist))
		}
		fmt.Fprintf(&b, "    INDEX 01 %02d:%02d:%02d\n", mins, remSec, frames)
	}
	return b.String()
}

// FormatMixcloud formats s in Mixcloud's pasteable timestamped tracklist format.
func FormatMixcloud(s Session) string {
	var b strings.Builder
	for _, e := range s.Entries {
		total := max(0, int(math.Round(e.OffsetSec)))
		var ts string
		if total >= 3600 {
			ts = fmt.Sprintf("%d:%02d:%02d", total/3600, (total%3600)/60, total%60)
		} else {
			ts = fmt.Sprintf("%02d:%02d", total/60, total%60)
		}
		fmt.Fprintf(&b, "%s %s\n", ts, displayArtistTitle(e.Artist, e.Title))
	}
	return b.String()
}

// MixcloudSections returns Mixcloud API upload section entries and form fields.
func MixcloudSections(s Session) ([]MixcloudSection, map[string]string) {
	secs := make([]MixcloudSection, 0, len(s.Entries))
	fields := make(map[string]string, len(s.Entries)*3+1)
	if s.Title != "" {
		fields["name"] = cleanField(s.Title)
	}
	for i, e := range s.Entries {
		start := max(0, int(math.Round(e.OffsetSec)))
		artist := cleanField(e.Artist)
		song := cleanField(e.Title)
		if song == "" {
			song = artist
		}
		secs = append(secs, MixcloudSection{
			Index:        i,
			StartTimeSec: start,
			Artist:       artist,
			Song:         song,
		})
		pfx := fmt.Sprintf("sections-%d-", i)
		fields[pfx+"artist"] = artist
		fields[pfx+"song"] = song
		fields[pfx+"start_time"] = fmt.Sprintf("%d", start)
	}
	return secs, fields
}

// WriteSidecars writes .json, .txt, .cue, and .mixcloud.txt atomically into dir.
func WriteSidecars(s Session, dir, baseName, audioFile string) error {
	if dir == "" || baseName == "" {
		return errors.New("tracklist: empty dir or baseName")
	}
	baseName = filepath.Base(cleanField(baseName))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	secs, fields := MixcloudSections(s)
	jsonPayload, err := json.MarshalIndent(map[string]any{
		"session":          s,
		"mixcloudSections": secs,
		"mixcloudForm":     fields,
	}, "", "  ")
	if err != nil {
		return err
	}
	files := map[string][]byte{
		baseName + ".json":         append(jsonPayload, '\n'),
		baseName + ".txt":          []byte(FormatText(s)),
		baseName + ".cue":          []byte(FormatCUE(s, audioFile)),
		baseName + ".mixcloud.txt": []byte(FormatMixcloud(s)),
	}
	for name, data := range files {
		dst := filepath.Join(dir, name)
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return nil
}
