// Package providers reads playlist metadata through public HTTP APIs and asks a
// service for the audio address the account is entitled to. It never decrypts
// media. Each instance belongs to one account.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Track struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Artist     string   `json:"artist"`
	Artists    []string `json:"artists,omitempty"`
	Album      string   `json:"album,omitempty"`
	ISRC       string   `json:"isrc,omitempty"`
	DurationMS int64    `json:"durationMs"`
	Provider   string   `json:"provider"`
	BPM        float64  `json:"bpm,omitempty"`
	Key        string   `json:"key,omitempty"` // Camelot, e.g. 8A
	// Preview is Deezer's public 30-second MP3, used only to estimate tempo.
	Preview string `json:"-"`
}
type Playlist struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Count int    `json:"count"`
}
type Provider interface {
	Playlists(context.Context) ([]Playlist, error)
	Tracks(context.Context, string) ([]Track, error)
}

const GuestPlaylist = "[Guest / 临时加歌]"
const OwnerPlaylist = "[Stream / 临时下载]"

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// JSON performs bounded HTTP I/O and deliberately omits response bodies from
// errors: authentication services sometimes echo cookies or access tokens.
func JSON(ctx context.Context, c *http.Client, method, endpoint string, body io.Reader, headers http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	resp, err := client(c).Do(req)
	if err != nil {
		return errors.New("provider: HTTP transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return errors.New("provider: response too large")
	}
	if err = json.Unmarshal(data, out); err != nil {
		return errors.New("provider: invalid JSON response")
	}
	return nil
}
func endpoint(base, path string, q url.Values) string {
	s := strings.TrimRight(base, "/") + path
	if len(q) > 0 {
		s += "?" + q.Encode()
	}
	return s
}

// SaveJSON replaces an owner credential file atomically with mode 0600. Guest
// credentials stay in memory and must never be passed here.
func SaveJSON(path string, v any) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".credential-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
