package deemixclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	// An audio host may take this long to start sending.
	mediaFirstByte = 90 * time.Second
	maxMediaBytes  = 2 << 30
)

// Only downloads this pipeline named are ever evicted: Save's and Deemix's <id>.
var evictable = regexp.MustCompile(`^(netease-|soundcloud-)?\d+\.(flac|mp3|mp4)$`)

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
	if !q.Lossless && !c.lossyAllowed() {
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
	path := filepath.Join(c.DownloadDir, name+".mp4")
	if isMP4(path) {
		return &Download{Path: path, Quality: Quality{Label: "[AAC]", Format: "mp4"}}
	}
	return nil
}

func isMP4(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var head [8]byte
	_, err = io.ReadFull(f, head[:])
	return err == nil && string(head[4:]) == "ftyp"
}

// SaveParts joins the segments of one stream into name.ext, keeping what is checked
// to be whole audio. Long DJ sets come this way, so a segment gets a few attempts.
func (c *Client) SaveParts(ctx context.Context, name, ext string, urls []string) (*Download, error) {
	if ext != "mp3" && ext != "mp4" {
		return nil, errors.New("audio: unsupported format")
	}
	if err := os.MkdirAll(c.DownloadDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(c.DownloadDir, ".part-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	for _, address := range urls {
		if err = c.appendPart(ctx, f, address); err != nil {
			return nil, err
		}
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	var dl *Download
	if ext == "mp4" {
		if !isMP4(f.Name()) {
			return nil, errors.New("audio: not an MP4 file")
		}
		dl = &Download{Quality: Quality{Label: "[AAC]", Format: "mp4"}}
	} else {
		q, err := Inspect(f.Name())
		if err != nil {
			return nil, err
		}
		if q.Format != "mp3" {
			return nil, errors.New("audio: not an MP3 file")
		}
		dl = &Download{Quality: q}
	}
	dl.Path = filepath.Join(c.DownloadDir, name+"."+ext)
	if err = os.Chmod(f.Name(), 0o644); err != nil {
		return nil, err
	}
	if err = os.Rename(f.Name(), dl.Path); err != nil {
		return nil, err
	}
	c.Trim(dl.Path)
	return dl, nil
}

// appendPart rewinds to where it started, so a failed attempt leaves no partial bytes.
func (c *Client) appendPart(ctx context.Context, f *os.File, address string) error {
	start, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return err
		}
		if err := f.Truncate(start); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
		if err != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
			return errors.New("audio: invalid address")
		}
		resp, err := c.media().Do(req)
		if err != nil {
			last = errors.New("audio: transfer failed")
			continue
		}
		var n int64
		if resp.StatusCode == http.StatusOK {
			n, err = io.Copy(f, io.LimitReader(resp.Body, maxMediaBytes))
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode != http.StatusOK:
			last = fmt.Errorf("audio: HTTP %d", resp.StatusCode)
		case err != nil || (resp.ContentLength >= 0 && n != resp.ContentLength):
			last = errors.New("audio: transfer interrupted")
		default:
			return nil
		}
	}
	return last
}

// Save stores the audio at address as name.flac or name.mp3, after checking what
// actually arrived. Addresses can carry access tokens, so errors never quote them.
func (c *Client) Save(ctx context.Context, name, address string) (*Download, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return nil, errors.New("audio: invalid address")
	}
	resp, err := c.media().Do(req)
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
	c.Trim(final)
	return dl, nil
}

// Trim removes the oldest downloads beyond MaxCache, never the one just saved.
func (c *Client) Trim(keep string) {
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
		if evictable.MatchString(e.Name()) && path != keep {
			files = append(files, file{path, info.Size(), info.ModTime()})
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
