package main

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const deckOnlineTTL = 2 * time.Minute

type deckInfo struct {
	ID         string    `json:"id"`                   // 唯一标识，如主机序列号/NetworkId 或 IP
	Name       string    `json:"name"`                 // 如 "SC5000 (Role 13)"
	Model      string    `json:"model"`                // 如 "SC5000"
	Version    string    `json:"version"`              // 如 "5.3.0.4"
	Role       string    `json:"role"`                 // 如 "13" / "24"
	LANIP      string    `json:"lanIp"`                // 如 "192.168.1.224"
	ConsoleURL string    `json:"consoleUrl"`           // 如 "http://192.168.1.224:8080"
	AccessCode string    `json:"accessCode,omitempty"` // 碟机本机访问码（如 ABCD-1234，不在公开接口暴露）
	LoggedIn   []string  `json:"loggedIn"`             // 已登录流媒体平台列表
	LastSeen   time.Time `json:"lastSeen"`
}

type deckView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Model      string    `json:"model"`
	Version    string    `json:"version"`
	Role       string    `json:"role"`
	LANIP      string    `json:"lanIp"`
	ConsoleURL string    `json:"consoleUrl"`
	LoggedIn   []string  `json:"loggedIn"`
	LastSeen   time.Time `json:"lastSeen"`
	Online     bool      `json:"online"`
}

func (d deckInfo) view(now time.Time) deckView {
	loggedIn := d.LoggedIn
	if loggedIn == nil {
		loggedIn = []string{}
	}
	return deckView{
		ID:         d.ID,
		Name:       d.Name,
		Model:      d.Model,
		Version:    d.Version,
		Role:       d.Role,
		LANIP:      d.LANIP,
		ConsoleURL: d.ConsoleURL,
		LoggedIn:   loggedIn,
		LastSeen:   d.LastSeen,
		Online:     !d.LastSeen.IsZero() && now.Sub(d.LastSeen) < deckOnlineTTL,
	}
}

type stemsStatus struct {
	Online    bool   `json:"online"`
	Device    string `json:"device,omitempty"`
	Backend   string `json:"backend,omitempty"`
	Separator string `json:"separator,omitempty"`
	Busy      bool   `json:"busy"`
	Sessions  int    `json:"sessions,omitempty"`
}

type deckStore struct {
	mu   sync.Mutex
	path string
	m    map[string]deckInfo
}

func newDeckStore(path string) *deckStore {
	ds := &deckStore{path: path, m: map[string]deckInfo{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return ds
	}
	var list []deckInfo
	if err := json.Unmarshal(b, &list); err == nil {
		for _, d := range list {
			if d.ID != "" {
				ds.m[d.ID] = d
			}
		}
		return ds
	}
	var mp map[string]deckInfo
	if err := json.Unmarshal(b, &mp); err == nil {
		for k, d := range mp {
			if d.ID == "" {
				d.ID = k
			}
			ds.m[d.ID] = d
		}
	} else {
		log.Printf("decks: ignoring unreadable %s: %v", path, err)
	}
	return ds
}

func (ds *deckStore) saveLocked() {
	if ds.path == "" {
		return
	}
	list := ds.sortedLocked()
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(ds.path, b, 0o600); err != nil {
		log.Printf("decks: save: %v", err)
	}
}

func (ds *deckStore) sortedLocked() []deckInfo {
	out := make([]deckInfo, 0, len(ds.m))
	for _, d := range ds.m {
		if d.LoggedIn == nil {
			d.LoggedIn = []string{}
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b deckInfo) int {
		if a.Role != b.Role {
			if a.Role == "" {
				return 1
			}
			if b.Role == "" {
				return -1
			}
			return cmp.Compare(a.Role, b.Role)
		}
		if a.LANIP != b.LANIP {
			return cmp.Compare(a.LANIP, b.LANIP)
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

func (ds *deckStore) listView(now time.Time) []deckView {
	if ds == nil {
		return []deckView{}
	}
	ds.mu.Lock()
	list := ds.sortedLocked()
	ds.mu.Unlock()
	out := make([]deckView, 0, len(list))
	for _, d := range list {
		out = append(out, d.view(now))
	}
	return out
}

func normalizeDeckCode(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "", "_", "").Replace(strings.TrimSpace(s)))
}

func (ds *deckStore) checkAccessCode(pw string) bool {
	if ds == nil {
		return false
	}
	norm := normalizeDeckCode(pw)
	if len(norm) < 4 || len(norm) > 64 {
		return false
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	matched := false
	for _, d := range ds.m {
		code := normalizeDeckCode(d.AccessCode)
		if code != "" && subtle.ConstantTimeCompare([]byte(norm), []byte(code)) == 1 {
			matched = true
		}
	}
	return matched
}

func (ds *deckStore) upsert(in deckInfo) deckInfo {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	in.ID = strings.TrimSpace(in.ID)
	in.LANIP = strings.TrimSpace(in.LANIP)
	in.Role = strings.TrimSpace(in.Role)
	in.Model = strings.TrimSpace(in.Model)
	in.Version = strings.TrimSpace(in.Version)
	in.Name = strings.TrimSpace(in.Name)
	in.ConsoleURL = strings.TrimSpace(in.ConsoleURL)
	in.AccessCode = strings.TrimSpace(in.AccessCode)

	if in.ID == "" {
		switch {
		case in.LANIP != "" && in.LANIP != "sc5000.lan" && in.LANIP != "enginex.lan": // placeholders, not addresses (#234)
			in.ID = in.LANIP
		case in.AccessCode != "":
			in.ID = "deck-" + strings.ToLower(normalizeDeckCode(in.AccessCode))
		case in.Role != "":
			in.ID = "role-" + in.Role
		case in.LANIP != "":
			in.ID = in.LANIP
		default:
			in.ID = "deck-default"
		}
	}

	old, exists := ds.m[in.ID]
	if !exists && in.AccessCode != "" {
		norm := normalizeDeckCode(in.AccessCode)
		for k, v := range ds.m {
			// Only migrate an entry when it has the same role (or no role/IP yet),
			// never when two decks in a cluster share the same access code.
			if norm != "" && normalizeDeckCode(v.AccessCode) == norm && (v.LANIP == "" || v.LANIP == in.LANIP) && (v.Role == "" || in.Role == "" || v.Role == in.Role) {
				old, exists = v, true
				if in.ID != k {
					delete(ds.m, k)
				}
				break
			}
		}
	}

	model := cmp.Or(in.Model, old.Model, "SC5000")
	role := cmp.Or(in.Role, old.Role)
	version := cmp.Or(in.Version, old.Version)
	lanIP := cmp.Or(in.LANIP, old.LANIP)
	code := cmp.Or(in.AccessCode, old.AccessCode)

	consoleURL := in.ConsoleURL
	if consoleURL == "" {
		if lanIP != "" {
			consoleURL = "http://" + lanIP + ":8080"
		} else {
			consoleURL = old.ConsoleURL
		}
	}

	name := in.Name
	if name == "" {
		if role != "" {
			name = fmt.Sprintf("%s (Role %s)", model, role)
		} else if old.Name != "" {
			name = old.Name
		} else {
			name = model
		}
	}

	loggedIn := in.LoggedIn
	if loggedIn == nil {
		if old.LoggedIn != nil {
			loggedIn = old.LoggedIn
		} else {
			loggedIn = []string{}
		}
	}

	updated := deckInfo{
		ID:         in.ID,
		Name:       name,
		Model:      model,
		Version:    version,
		Role:       role,
		LANIP:      lanIP,
		ConsoleURL: consoleURL,
		AccessCode: code,
		LoggedIn:   loggedIn,
		LastSeen:   time.Now().UTC(),
	}
	ds.m[updated.ID] = updated
	ds.saveLocked()
	return updated
}

func hasDeckHeaders(h http.Header) bool {
	for _, k := range []string{
		"X-EngineX-Deck-ID",
		"X-EngineX-Deck-IP",
		"X-EngineX-Deck-Role",
		"X-EngineX-Deck-Version",
		"X-EngineX-Deck-Code",
		"X-EngineX-Deck-LoggedIn",
		"X-EngineX-Deck-Model",
		"X-EngineX-Deck-Name",
	} {
		if len(h.Values(k)) > 0 && strings.TrimSpace(h.Get(k)) != "" {
			return true
		}
	}
	return len(h.Values("X-EngineX-Deck-LoggedIn")) > 0
}

func parseLoggedInHeader(raw string) []string {
	out := []string{}
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func deckFromHeaders(r *http.Request) deckInfo {
	h := r.Header
	d := deckInfo{
		ID:         strings.TrimSpace(h.Get("X-EngineX-Deck-ID")),
		Name:       strings.TrimSpace(h.Get("X-EngineX-Deck-Name")),
		Model:      strings.TrimSpace(h.Get("X-EngineX-Deck-Model")),
		Version:    strings.TrimSpace(h.Get("X-EngineX-Deck-Version")),
		Role:       strings.TrimSpace(h.Get("X-EngineX-Deck-Role")),
		LANIP:      strings.TrimSpace(h.Get("X-EngineX-Deck-IP")),
		AccessCode: strings.TrimSpace(h.Get("X-EngineX-Deck-Code")),
	}
	if len(h.Values("X-EngineX-Deck-LoggedIn")) > 0 {
		d.LoggedIn = parseLoggedInHeader(h.Get("X-EngineX-Deck-LoggedIn"))
	}
	return d
}

func (ds *deckStore) observeRequest(r *http.Request) (deckInfo, bool) {
	if ds == nil || !hasDeckHeaders(r.Header) {
		return deckInfo{}, false
	}
	d := deckFromHeaders(r)
	if d.LANIP == "" {
		if ip := clientIP(r); ip != "" && ip != "127.0.0.1" && ip != "::1" {
			d.LANIP = ip
		}
	}
	return ds.upsert(d), true
}

func (s *server) handleDeckPost(w http.ResponseWriter, r *http.Request) {
	d := deckFromHeaders(r)
	if r.Body != nil {
		var body deckInfo
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		if err := dec.Decode(&body); err != nil && err != io.EOF {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "invalid json"})
			return
		}
		d.ID = cmp.Or(strings.TrimSpace(body.ID), d.ID)
		d.Name = cmp.Or(strings.TrimSpace(body.Name), d.Name)
		d.Model = cmp.Or(strings.TrimSpace(body.Model), d.Model)
		d.Version = cmp.Or(strings.TrimSpace(body.Version), d.Version)
		d.Role = cmp.Or(strings.TrimSpace(body.Role), d.Role)
		d.LANIP = cmp.Or(strings.TrimSpace(body.LANIP), d.LANIP)
		d.ConsoleURL = cmp.Or(strings.TrimSpace(body.ConsoleURL), d.ConsoleURL)
		d.AccessCode = cmp.Or(strings.TrimSpace(body.AccessCode), d.AccessCode)
		if body.LoggedIn != nil {
			d.LoggedIn = body.LoggedIn
		}
	}
	if d.LANIP == "" {
		if ip := clientIP(r); ip != "" && ip != "127.0.0.1" && ip != "::1" {
			d.LANIP = ip
		}
	}
	saved := s.decks.upsert(d)
	now := time.Now()
	writeJSON(w, 200, map[string]any{
		"ok":    true,
		"deck":  saved.view(now),
		"decks": s.decks.listView(now),
	})
}

func (s *server) stemsStatus(ctx context.Context) stemsStatus {
	if s.stems == nil || s.stems.target == nil {
		return stemsStatus{Online: false}
	}
	token, err := s.stems.token()
	if err != nil {
		return stemsStatus{Online: false}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, "GET", s.stems.url("/healthz"), nil)
	if err != nil {
		return stemsStatus{Online: false}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.stems.transport.RoundTrip(req)
	if err != nil {
		return stemsStatus{Online: false}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return stemsStatus{Online: false}
	}
	var h struct {
		OK        bool   `json:"ok"`
		Device    string `json:"device"`
		Backend   string `json:"backend"`
		Separator string `json:"separator"`
		Busy      bool   `json:"busy"`
		Sessions  int    `json:"sessions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h); err != nil || !h.OK {
		return stemsStatus{Online: false}
	}
	return stemsStatus{
		Online:    true,
		Device:    h.Device,
		Backend:   h.Backend,
		Separator: h.Separator,
		Busy:      h.Busy,
		Sessions:  h.Sessions,
	}
}
