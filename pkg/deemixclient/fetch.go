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
	"syscall"
	"time"
)

const (
	// An audio host may take this long to start sending.
	mediaFirstByte = 90 * time.Second
	maxMediaBytes  = 2 << 30
	maxPartBytes   = 64 << 20
	// The deck's link fills at about 16 requests; 8 in lockstep got half of it.
	partsAtOnce = 16
)

// Only downloads this pipeline named are ever evicted: Save's and Deemix's <id>.
var evictable = regexp.MustCompile(`^(netease-|soundcloud-)?\d+\.(flac|mp3|mp4)$`)

var defaultMedia = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = mediaFirstByte
	t.MaxIdleConnsPerHost = partsAtOnce
	return &http.Client{Transport: t}
}()

func (c *Client) media() *http.Client {
	if c.Media != nil {
		return c.Media
	}
	return defaultMedia
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
	if err = c.fetchParts(ctx, f, wholeParts(urls)); err != nil {
		return nil, err
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

// part is one request of a download: a whole address, or bytes from..to of it.
type part struct {
	url      string
	from, to int64 // to is inclusive; from < 0 asks for the whole response
}

func wholeParts(urls []string) []part {
	parts := make([]part, len(urls))
	for i, u := range urls {
		parts[i] = part{u, -1, -1}
	}
	return parts
}

// fetchParts keeps partsAtOnce requests going and writes the answers in order. A
// slow part holds back only its own slot, never a whole batch, and no more than
// partsAtOnce answers wait in memory.
func (c *Client) fetchParts(ctx context.Context, f io.Writer, parts []part) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type answer struct {
		data []byte
		err  error
	}
	answers := make([]chan answer, len(parts))
	for i := range answers {
		answers[i] = make(chan answer, 1)
	}
	slots := make(chan struct{}, partsAtOnce)
	go func() {
		for i, p := range parts {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				data, err := c.fetchPart(ctx, p)
				answers[i] <- answer{data, err}
			}()
		}
	}()
	for i := range parts {
		var a answer
		select {
		case a = <-answers[i]:
		case <-ctx.Done():
			return ctx.Err()
		}
		if a.err != nil {
			return a.err
		}
		if _, err := f.Write(a.data); err != nil {
			return err
		}
		<-slots
	}
	return nil
}

func (c *Client) fetchPart(ctx context.Context, p part) ([]byte, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", p.url, nil)
		if err != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
			return nil, errors.New("audio: invalid address")
		}
		want := http.StatusOK
		if p.from >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", p.from, p.to))
			want = http.StatusPartialContent
		}
		resp, err := c.media().Do(req)
		if err != nil {
			last = errors.New("audio: transfer failed")
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxPartBytes))
		resp.Body.Close()
		switch {
		case resp.StatusCode != want:
			last = fmt.Errorf("audio: HTTP %d", resp.StatusCode)
		case err != nil || (resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength):
			last = errors.New("audio: transfer interrupted")
		case p.from >= 0 && int64(len(data)) != p.to-p.from+1:
			last = errors.New("audio: transfer interrupted")
		default:
			return data, nil
		}
	}
	return nil, last
}

// splitBytes is the size of one ranged request; partsAtOnce of them fit in memory.
const splitBytes = 2 << 20

// fetchSplit downloads one file as parallel byte ranges. A host that ignores
// Range just sends it whole, which is kept as it comes.
func (c *Client) fetchSplit(ctx context.Context, f io.Writer, address string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return errors.New("audio: invalid address")
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", splitBytes-1))
	resp, err := c.media().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("audio: transfer failed")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		n, err := io.Copy(f, io.LimitReader(resp.Body, maxMediaBytes+1))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("audio: transfer interrupted")
		}
		if n > maxMediaBytes || (resp.ContentLength >= 0 && n != resp.ContentLength) {
			return errors.New("audio: incomplete or oversized transfer")
		}
		return nil
	case http.StatusPartialContent:
	default:
		return fmt.Errorf("audio: HTTP %d", resp.StatusCode)
	}
	var from, to, total int64
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &from, &to, &total); err != nil || from != 0 || to < 0 || to >= total || total > maxMediaBytes {
		return errors.New("audio: unexpected byte range")
	}
	first, err := io.ReadAll(io.LimitReader(resp.Body, splitBytes+1))
	if err != nil || int64(len(first)) != to+1 {
		return errors.New("audio: transfer interrupted")
	}
	if _, err := f.Write(first); err != nil {
		return err
	}
	var parts []part
	for at := to + 1; at < total; at += splitBytes {
		parts = append(parts, part{address, at, min(at+splitBytes, total) - 1})
	}
	return c.fetchParts(ctx, f, parts)
}

// SaveSplit stores the file at address as name.flac or name.mp3, fetched as
// parallel byte ranges. WAV and AIFF are re-encoded to FLAC with the same samples.
// The caller decides whether MP3 is acceptable; this keeps whichever arrived.
func (c *Client) SaveSplit(ctx context.Context, name, address string) (*Download, error) {
	if err := os.MkdirAll(c.DownloadDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(c.DownloadDir, ".part-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = c.fetchSplit(ctx, f, address); err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	path := f.Name()
	if converted, err := pcmToFLAC(path); err == nil {
		defer os.Remove(converted)
		path = converted
	} else if !errors.Is(err, errNotPCM) {
		return nil, err
	}
	q, err := Inspect(path)
	if err != nil {
		return nil, err
	}
	final := filepath.Join(c.DownloadDir, name+"."+q.Format)
	if err = os.Chmod(path, 0o644); err != nil {
		return nil, err
	}
	if err = os.Rename(path, final); err != nil {
		return nil, err
	}
	// Only one of name's formats is kept, so Cached finds the one just saved.
	for _, ext := range []string{".flac", ".mp3", ".mp4"} {
		if other := filepath.Join(c.DownloadDir, name+ext); other != final {
			os.Remove(other)
		}
	}
	c.Trim(final)
	return &Download{Path: final, Quality: q}, nil
}

// RemoveStaleParts clears unfinished downloads left by a previous run.
func (c *Client) RemoveStaleParts() {
	parts, _ := filepath.Glob(filepath.Join(c.DownloadDir, ".part-*"))
	for _, p := range parts {
		os.Remove(p)
	}
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

// Trim removes the least recently used downloads while they exceed MaxCache or
// the volume has less than MinFree left, never the one just saved.
func (c *Client) Trim(keep string) {
	if c.MaxCache <= 0 {
		return
	}
	short := func() bool { return false }
	if c.MinFree > 0 {
		short = func() bool {
			var st syscall.Statfs_t
			return syscall.Statfs(c.DownloadDir, &st) == nil && int64(st.Bavail)*int64(st.Bsize) < c.MinFree
		}
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
		if total <= c.MaxCache && !short() {
			return
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
