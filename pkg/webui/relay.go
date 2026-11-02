package webui

// The cloud relay lets a phone on any network finish a Spotify or Deezer login.
// The deck registers the QR code's one-time token, then polls the relay with it
// until the phone is done and collects the resulting login. The token is the only
// thing that links the two, and it is shown only on the deck's screen.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
)

// DefaultRelayURL is the operator's own login service.
const DefaultRelayURL = "https://relay.example.com"

const relayTimeout = 3 * time.Second

var errRelayIncomplete = errors.New("relay returned an incomplete login")

type relaySession struct {
	Status       string           `json:"status"`
	SpotifyToken *providers.Token `json:"spotifyToken"`
	Deezer       *struct {
		UserID string `json:"userId"`
		ARL    string `json:"arl"`
	} `json:"deezer"`
	SoundCloud *struct {
		User     string `json:"user"`
		ClientID string `json:"clientId"`
		Token    string `json:"token"`
	} `json:"soundcloud"`
}

// relayCall makes one JSON request. Redirects are refused so that the password,
// a custom header Go would carry along, cannot reach another host.
func (s *Server) relayCall(ctx context.Context, method, path string, body io.Reader, out any) error {
	base, password := s.relaySettings()
	h := http.Header{"Content-Type": {"application/json"}}
	if password != "" {
		h.Set("X-Deemix-Password", password)
		h.Set("Authorization", "Bearer "+password)
	}
	c := http.Client{Timeout: relayTimeout}
	if s.cfg.HTTP != nil {
		c = *s.cfg.HTTP
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return providers.JSON(ctx, &c, method, strings.TrimRight(base, "/")+path, body, h, out)
}

func (s *Server) relayPage(flow *loginFlow) string {
	base, _ := s.relaySettings()
	return strings.TrimRight(base, "/") + "/m/" + flow.Provider + "?t=" + url.QueryEscape(flow.token)
}

// relayRegister tells the relay about a new QR code. Failure is only logged.
func (s *Server) relayRegister(ctx context.Context, flow *loginFlow) {
	ctx, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"token": flow.token, "provider": flow.Provider, "spotifyClientId": s.spotifyClientID()})
	if err != nil {
		return
	}
	var out struct{}
	if err = s.relayCall(ctx, "POST", "/api/relay/session", strings.NewReader(string(body)), &out); err != nil {
		log.Printf("stream-sync: relay registration failed: %v", err)
	}
}

// relayStatus returns the flow's status after asking the relay. Anything that
// goes wrong leaves it as it was; the deck polls again shortly.
func (s *Server) relayStatus(ctx context.Context, session *Session, token, status string) string {
	if token == "" {
		return status
	}
	ctx, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()
	var v relaySession
	if err := s.relayCall(ctx, "GET", "/api/relay/session?t="+url.QueryEscape(token), nil, &v); err != nil {
		return status
	}
	switch v.Status {
	case "scanned":
		return "scanned"
	case "done":
		if err := s.relayCollect(session, v); err != nil {
			log.Printf("stream-sync: relay login not saved: %v", err)
			return status
		}
		return "done"
	}
	return status
}

func (s *Server) relayCollect(session *Session, v relaySession) error {
	switch p := session.p.(type) {
	case *providers.SoundCloud:
		if v.SoundCloud == nil {
			return errRelayIncomplete
		}
		// The relay never holds a working client, so the deck checks what it sent.
		ctx, cancel := context.WithTimeout(context.Background(), 2*relayTimeout)
		defer cancel()
		return s.linkSoundCloud(ctx, session, v.SoundCloud.User, v.SoundCloud.ClientID, v.SoundCloud.Token)
	case *providers.Spotify:
		if v.SpotifyToken == nil {
			return errRelayIncomplete
		}
		return p.SetToken(*v.SpotifyToken)
	case *providers.Deezer:
		if v.Deezer == nil || !providers.IsDeezerUserID(v.Deezer.UserID) {
			return errRelayIncomplete
		}
		return s.saveDeezerOwner(p, deezerOwner{v.Deezer.UserID, v.Deezer.ARL})
	}
	return errRelayIncomplete
}

// bpmSyncInterval paces the pull of BPM service keys saved on the relay's console.
const bpmSyncInterval = time.Hour

// SyncRelaySettings keeps the BPM service keys in step with the relay until ctx ends.
func (s *Server) SyncRelaySettings(ctx context.Context) {
	for {
		if err := s.pullBPMConfig(ctx); err != nil {
			log.Printf("stream-sync: BPM settings not synced: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(bpmSyncInterval):
		}
	}
}

// pullBPMConfig copies keys set on the relay. A key the relay does not have is left
// alone, so one entered on this deck's own console survives.
func (s *Server) pullBPMConfig(ctx context.Context) error {
	if s.cfg.BPM == nil {
		return nil
	}
	if base, password := s.relaySettings(); base == "" || password == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()
	var r struct {
		Config providers.BPMConfig `json:"config"`
	}
	if err := s.relayCall(ctx, "GET", "/api/config?secrets=1", nil, &r); err != nil {
		return err
	}
	v := s.cfg.BPM.Config()
	next := v
	if r.Config.SoundCloudClientID != "" {
		next.SoundCloudClientID = r.Config.SoundCloudClientID
	}
	if r.Config.SoundCloudToken != "" {
		next.SoundCloudToken = r.Config.SoundCloudToken
	}
	if r.Config.GetSongBPMKey != "" {
		next.GetSongBPMKey = r.Config.GetSongBPMKey
	}
	if next == v {
		return nil
	}
	return s.cfg.BPM.SaveConfig(next)
}
