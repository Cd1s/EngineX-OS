package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Cd1s/EngineX-OS/pkg/broadcast"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

var (
	broadcasters  sync.Map // map[*Server]*broadcast.Broadcaster
	broadcasterMu sync.Mutex
)

// broadcaster returns the lazily initialized Broadcaster for this server.
func (s *Server) broadcaster() *broadcast.Broadcaster {
	if v, ok := broadcasters.Load(s); ok {
		return v.(*broadcast.Broadcaster)
	}

	broadcasterMu.Lock()
	defer broadcasterMu.Unlock()
	if v, ok := broadcasters.Load(s); ok {
		return v.(*broadcast.Broadcaster)
	}

	var guard *resourceguard.Guard
	if s.jobs != nil && s.jobs.p.Guard != nil {
		guard = s.jobs.p.Guard
	}
	b := broadcast.New(guard)

	cfg := s.loadBroadcastConfig()
	if cfg.Enabled && cfg.ServerURL != "" {
		_ = b.Configure(cfg)
	}

	broadcasters.Store(s, b)
	return b
}

func (s *Server) broadcastConfigPath() string {
	return filepath.Join(s.cfg.StateDir, "broadcast-config.json")
}

func (s *Server) loadBroadcastConfig() broadcast.Config {
	var cfg broadcast.Config
	if b, err := os.ReadFile(s.broadcastConfigPath()); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	return cfg
}

func broadcastHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(raw)
	}
	return strings.ToLower(u.Host)
}

// broadcastGet handles GET /api/broadcast.
func (s *Server) broadcastGet(w http.ResponseWriter, r *http.Request) {
	b := s.broadcaster()
	respond(w, b.Status())
}

type broadcastPostInput struct {
	Enabled    *bool   `json:"enabled"`
	Protocol   *string `json:"protocol"`
	ServerURL  *string `json:"serverUrl"`
	Mountpoint *string `json:"mountpoint"`
	Username   *string `json:"username"`
	Password   *string `json:"password"`
	Format     *string `json:"format"`
	Bitrate    *int    `json:"bitrate"`
	StreamName *string `json:"streamName"`
	InputPath  *string `json:"inputPath"`
}

// broadcastPost handles POST /api/broadcast.
func (s *Server) broadcastPost(w http.ResponseWriter, r *http.Request) {
	b := s.broadcaster()
	s.uiMu.Lock()
	defer s.uiMu.Unlock()

	cfg := s.loadBroadcastConfig()

	// 1. Handle query parameter ?enabled=1|0 or true|false
	if q := r.URL.Query().Get("enabled"); q != "" {
		if q == "1" || strings.EqualFold(q, "true") {
			cfg.Enabled = true
		} else if q == "0" || strings.EqualFold(q, "false") {
			cfg.Enabled = false
		}
	}

	// 2. Handle optional JSON body
	if r.Body != nil {
		bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
		if err != nil {
			fail(w, 400, errors.New("request body too large"))
			return
		}
		if len(bytes.TrimSpace(bodyBytes)) > 0 {
			var in broadcastPostInput
			d := json.NewDecoder(bytes.NewReader(bodyBytes))
			d.DisallowUnknownFields()
			if err := d.Decode(&in); err != nil {
				fail(w, 400, errors.New("invalid JSON request"))
				return
			}

			if in.Enabled != nil {
				cfg.Enabled = *in.Enabled
			}
			if in.ServerURL != nil {
				newHost := broadcastHost(*in.ServerURL)
				oldHost := broadcastHost(cfg.ServerURL)
				if oldHost != "" && newHost != oldHost && in.Password == nil {
					cfg.Password = ""
				}
				cfg.ServerURL = *in.ServerURL
			}
			if in.Password != nil {
				cfg.Password = *in.Password
			}
			if in.Protocol != nil {
				cfg.Protocol = *in.Protocol
			}
			if in.Mountpoint != nil {
				cfg.Mountpoint = *in.Mountpoint
			}
			if in.Username != nil {
				cfg.Username = *in.Username
			}
			if in.Format != nil {
				cfg.Format = *in.Format
			}
			if in.Bitrate != nil {
				cfg.Bitrate = *in.Bitrate
			}
			if in.StreamName != nil {
				cfg.StreamName = *in.StreamName
			}
			if in.InputPath != nil {
				cfg.InputPath = *in.InputPath
			}
		}
	}

	if err := providers.SaveJSON(s.broadcastConfigPath(), cfg); err != nil {
		fail(w, 500, errors.New("could not save broadcast settings"))
		return
	}
	if err := b.Configure(cfg); err != nil {
		fail(w, 400, err)
		return
	}
	respond(w, b.Status())
}

// broadcastMetadataPost handles POST /api/broadcast/metadata.
func (s *Server) broadcastMetadataPost(w http.ResponseWriter, r *http.Request) {
	b := s.broadcaster()

	artist := r.URL.Query().Get("artist")
	title := r.URL.Query().Get("title")

	if r.Body != nil {
		bodyBytes, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
		if len(bytes.TrimSpace(bodyBytes)) > 0 {
			var in struct {
				Artist string `json:"artist"`
				Title  string `json:"title"`
				Deck   int    `json:"deck"`
			}
			if err := json.Unmarshal(bodyBytes, &in); err == nil {
				if in.Artist != "" && artist == "" {
					artist = in.Artist
				}
				if in.Title != "" && title == "" {
					title = in.Title
				}
			}
		}
	}

	_ = b.UpdateMetadata(artist, title)
	respond(w, b.Status())
}

// BroadcastEnabled reports whether broadcast streaming is enabled.
func (s *Server) BroadcastEnabled() bool {
	return s.broadcaster().Status().Enabled
}

// SetBroadcastEnabled toggles broadcast streaming and persists the preference.
func (s *Server) SetBroadcastEnabled(on bool) error {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()

	cfg := s.loadBroadcastConfig()
	cfg.Enabled = on
	if err := providers.SaveJSON(s.broadcastConfigPath(), cfg); err != nil {
		return err
	}
	return s.broadcaster().SetEnabled(on)
}

// RegisterBroadcastRoutes registers broadcast routes on the server's mux.
func (s *Server) RegisterBroadcastRoutes() {
	s.mux.HandleFunc("GET /api/broadcast", s.broadcastGet)
	s.mux.HandleFunc("POST /api/broadcast", s.broadcastPost)
	s.mux.HandleFunc("POST /api/broadcast/metadata", s.broadcastMetadataPost)
}
