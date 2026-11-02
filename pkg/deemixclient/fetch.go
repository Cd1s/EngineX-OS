package deemixclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
)

const (
	// A service may take this long to produce a track before it starts sending.
	mediaFirstByte = 90 * time.Second
	maxMediaBytes  = 2 << 30
)

var (
	unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	// Only files named by Save are ever evicted.
	savedPrefixes = []string{"netease-", "remote-"}
)

func (c *Client) media() *http.Client {
	if c.Media != nil {
		return c.Media
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = mediaFirstByte
	return &http.Client{Transport: t}
}

func (c *Client) accept(path string) (*Download, error) {
	q, err := Inspect(path)
	if err != nil {
		return nil, err
	}
	if !q.Lossless && !c.AllowLossy {
		return nil, errors.New("deemix: MP3 fallback needs explicit --allow-320k")
	}
	return &Download{Path: path, Quality: q}, nil
}

// Cached returns an earlier Save under name, so a restart does not fetch it again.
func (c *Client) Cached(name string) *Download {
	for _, ext := range []string{".flac", ".mp3"} {
		if dl, err := c.accept(filepath.Join(c.DownloadDir, name+ext)); err == nil {
			return dl
		}
	}
	return nil
}

// Save stores the audio at address as name.flac or name.mp3, after checking what
// actually arrived. Addresses can carry access tokens, so errors never quote them.
func (c *Client) Save(ctx context.Context, name, address string) (*Download, error) {
	return c.save(ctx, name, address, "")
}

func (c *Client) save(ctx context.Context, name, address, password string) (*Download, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return nil, errors.New("audio: invalid address")
	}
	media := c.media()
	if password != "" {
		req.Header.Set("X-Deemix-Password", password)
		req.Header.Set("Authorization", "Bearer "+password)
		// Go keeps custom headers across redirects, so stay on the backend's host.
		pinned := *media
		pinned.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			if next.URL.Host != via[0].URL.Host || len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		}
		media = &pinned
	}
	resp, err := media.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("audio: transfer failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audio: HTTP %d", resp.StatusCode)
	}
	if err = os.MkdirAll(c.DownloadDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(c.DownloadDir, ".part-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxMediaBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("audio: transfer interrupted")
	}
	if n > maxMediaBytes || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		return nil, errors.New("audio: incomplete or oversized transfer")
	}
	dl, err := c.accept(f.Name())
	if err != nil {
		return nil, err
	}
	final := filepath.Join(c.DownloadDir, name+"."+dl.Quality.Format)
	if err = os.Chmod(f.Name(), 0o644); err != nil {
		return nil, err
	}
	if err = os.Rename(f.Name(), final); err != nil {
		return nil, err
	}
	dl.Path = final
	c.trim(final)
	return dl, nil
}

// ErrNoRemote means no remote backend is configured.
var ErrNoRemote = errors.New("audio: no remote backend configured")

// FetchRemote asks the remote backend for a track by its metadata. The backend
// streams the audio back; the format is taken from the bytes, not from any header.
func (c *Client) FetchRemote(ctx context.Context, t providers.Track) (*Download, error) {
	if c.Remote == nil {
		return nil, ErrNoRemote
	}
	base, password := c.Remote()
	if base == "" {
		return nil, ErrNoRemote
	}
	name := "remote-" + unsafeName.ReplaceAllString(t.Provider+"-"+t.ID, "_")
	if dl := c.Cached(name); dl != nil {
		return dl, nil
	}
	q := url.Values{"provider": {t.Provider}, "id": {t.ID}, "isrc": {t.ISRC}, "title": {t.Title}, "artist": {t.Artist}, "duration": {strconv.FormatInt(t.DurationMS/1000, 10)}}
	return c.save(ctx, name, strings.TrimRight(base, "/")+"/api/audio?"+q.Encode(), password)
}

// trim removes the oldest saved files beyond MaxCache, never the one just saved.
func (c *Client) trim(keep string) {
	if c.MaxCache <= 0 {
		return
	}
	entries, err := os.ReadDir(c.DownloadDir)
	if err != nil {
		return
	}
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
		path := filepath.Join(c.DownloadDir, e.Name())
		for _, prefix := range savedPrefixes {
			if strings.HasPrefix(e.Name(), prefix) && path != keep {
				files = append(files, file{path, info.Size(), info.ModTime()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= c.MaxCache {
			return
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
