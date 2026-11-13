package deemixclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

const maxTrackedLRU = 512

var (
	queueURLToken   = regexp.MustCompile(`(?i)https?://\S+`)
	queueBareSecret = regexp.MustCompile(`\b(?:[0-9a-fA-F]{32,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_.-]+)\b`)
)

func sanitizeQueueError(msg string) string {
	msg = queueURLToken.ReplaceAllString(msg, "<url>")
	msg = queueBareSecret.ReplaceAllString(msg, "<redacted>")
	msg = strings.Join(strings.Fields(msg), " ")
	msg = strings.TrimPrefix(msg, "deemix: ")
	if runes := []rune(msg); len(runes) > 256 {
		msg = string(runes[:256])
	}
	return strings.TrimSpace(msg)
}

type Client struct {
	// Mode is "queue" (REST bridge) or "directory" (POST /api/download,
	// then watch exact <Deezer ID>.flac/.mp3 final names).
	Mode         string
	BaseURL      string
	QueuePath    string
	DownloadDir  string
	HTTP         *http.Client
	PollInterval time.Duration
	// AllowLossy must be explicitly enabled to accept MP3 from a FLAC request.
	AllowLossy bool
	// Media fetches audio files; the default has no overall deadline.
	Media *http.Client
	// PreferMP3 asks for 320k MP3 instead of FLAC; nil means FLAC. It is read per
	// download so that the deck's setting applies at once.
	PreferMP3 func() bool
	// MaxCache bounds the files this client saved itself; 0 means no limit.
	MaxCache int64
	// MinFree also evicts while the volume has less free space (needs MaxCache).
	MinFree int64

	mu       sync.Mutex
	lastUsed map[string]time.Time
}
type QueueStatus struct {
	ID       string  `json:"id"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Path     string  `json:"path"`
	Error    string  `json:"error"`
}
type Download struct {
	Path    string
	Quality Quality
}

func (c *Client) wantsMP3() bool { return c.PreferMP3 != nil && c.PreferMP3() }

// lossyAllowed also holds when MP3 is what was asked for.
func (c *Client) lossyAllowed() bool { return c.AllowLossy || c.wantsMP3() }

// request names the wanted quality in the download service's terms.
func (c *Client) request() (bitrate int, quality string) {
	if c.wantsMP3() {
		return 3, "MP3_320"
	}
	return 9, "FLAC"
}

// Lossless reports whether the next download should try for FLAC first.
func (c *Client) Lossless() bool { return !c.wantsMP3() }

func UnixHTTP(socket string) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}
func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "http://127.0.0.1:6595"
}
func (c *Client) queue() string {
	if c.QueuePath != "" {
		return "/" + strings.Trim(c.QueuePath, "/")
	}
	return "/api/queue"
}

// SafePath rejects queue paths outside the configured SD download tree,
// including symlink escapes. The queue must report an existing regular file.
func (c *Client) SafePath(path string) (string, error) {
	root, err := filepath.EvalSymlinks(c.DownloadDir)
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("deemix: result outside download directory")
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Size() == 0 {
		return "", errors.New("deemix: result is not a nonempty regular file")
	}
	return p, nil
}

// Download enqueues FLAC and polls the local REST/IPC bridge described in
// docs/stream-sync.md. Native Deemix Socket.IO is intentionally not guessed.
func (c *Client) Download(ctx context.Context, t providers.Track, progress func(QueueStatus)) (*Download, error) {
	if t.Provider != "deezer" || t.ID == "" {
		return nil, errors.New("deemix: resolve a Deezer track first")
	}
	if id, e := strconv.ParseUint(t.ID, 10, 64); e != nil || id == 0 {
		return nil, errors.New("deemix: invalid numeric track ID")
	}
	// Any verified <id>.flac/.mp3 already on disk is the track: a restart or a
	// second request must not download it again. A FLAC wish does not refetch a
	// cached MP3, which is usually there because Deezer had no FLAC.
	if dl := c.Cached(t.ID); dl != nil {
		return dl, nil
	}
	if c.Mode == "directory" {
		dl, err := c.downloadDirectory(ctx, t, progress)
		if err == nil && dl != nil {
			c.rememberActive(dl.Path)
		}
		return dl, err
	}
	if c.Mode != "" && c.Mode != "queue" {
		return nil, errors.New("deemix: unsupported adapter mode")
	}
	bitrate, name := c.request()
	data, _ := json.Marshal(map[string]any{"url": "https://www.deezer.com/track/" + url.PathEscape(t.ID), "bitrate": bitrate, "quality": name, "allowLossy": c.lossyAllowed()})
	var q QueueStatus
	if err := providers.JSON(ctx, c.HTTP, "POST", c.base()+c.queue(), strings.NewReader(string(data)), http.Header{"Content-Type": {"application/json"}}, &q); err != nil {
		return nil, err
	}
	if q.ID == "" || strings.ContainsAny(q.ID, "/?#") {
		return nil, errors.New("deemix: invalid queue ID")
	}
	jobID := q.ID
	interval := c.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if q.Error != "" {
			if progress != nil {
				progress(q)
			}
			if detail := sanitizeQueueError(q.Error); detail != "" {
				return nil, fmt.Errorf("deemix: %s", detail)
			}
			return nil, errors.New("deemix: queue reported an error; check CantStream/FLAC availability")
		}
		if progress != nil {
			progress(q)
		}
		switch strings.ToLower(q.State) {
		case "error", "failed", "cancelled", "cantstream":
			return nil, fmt.Errorf("deemix: download failed (%s); FLAC may be unavailable/CantStream", q.State)
		case "completed", "complete", "done":
			p, err := c.SafePath(q.Path)
			if err != nil {
				return nil, err
			}
			quality, err := Inspect(p)
			if err != nil {
				return nil, err
			}
			if !quality.Lossless && !c.lossyAllowed() {
				return nil, errors.New("deemix: MP3 fallback needs explicit --allow-320k")
			}
			c.rememberActive(p)
			return &Download{Path: p, Quality: quality}, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		if err := providers.JSON(ctx, c.HTTP, "GET", c.base()+c.queue()+"/"+url.PathEscape(jobID), nil, nil, &q); err != nil {
			return nil, err
		}
	}
}

// DownloadAs is Download for a service that keeps its own files: the result is
// name.flac/.mp3, never the shared <Deezer ID> file (a hard link, so a track that
// Deezer already holds costs no space or time, and trimming one name keeps the other).
func (c *Client) DownloadAs(ctx context.Context, t providers.Track, name string, progress func(QueueStatus)) (*Download, error) {
	if name == "" || filepath.Base(name) != name || strings.HasPrefix(name, ".") {
		return nil, errors.New("deemix: invalid file name")
	}
	if dl := c.Cached(name); dl != nil {
		return dl, nil
	}
	src, err := c.Download(ctx, t, progress)
	if err != nil {
		return nil, err
	}
	return c.LinkAs(src, name)
}

// LinkAs gives the file of src a second name, name.flac/.mp3 (a hard link, or a
// copy where links are not possible).
func (c *Client) LinkAs(src *Download, name string) (*Download, error) {
	if name == "" || filepath.Base(name) != name || strings.HasPrefix(name, ".") {
		return nil, errors.New("deemix: invalid file name")
	}
	dst := filepath.Join(c.DownloadDir, name+filepath.Ext(src.Path))
	if dst == src.Path {
		return src, nil
	}
	tmp := filepath.Join(c.DownloadDir, ".part-as-"+name)
	os.Remove(tmp)
	if err := os.Link(src.Path, tmp); err != nil {
		if err = copyFile(src.Path, tmp); err != nil {
			os.Remove(tmp)
			return nil, err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	c.removeOtherFormats(name, dst)
	c.rememberActive(src.Path)
	c.rememberActive(dst)
	c.Trim(dst, src.Path)
	return &Download{Path: dst, Quality: src.Quality}, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// downloadDirectory supports services with a submission endpoint but no HTTP
// queue status API. Configure their output template as <id>.<ext> and publish
// final names only after download completion. Never select an unrelated newest
// file: exact catalog ID, containment, stability and actual headers are required.
func (c *Client) downloadDirectory(ctx context.Context, t providers.Track, progress func(QueueStatus)) (*Download, error) {
	bitrate, _ := c.request()
	payload, _ := json.Marshal(map[string]any{"url": "https://www.deezer.com/track/" + t.ID, "bitrate": bitrate, "allowLossy": c.lossyAllowed()})
	req, err := http.NewRequestWithContext(ctx, "POST", c.base()+"/api/download", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return nil, errors.New("deemix: submission transport failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || len(raw) > 1<<20 {
		return nil, errors.New("deemix: submission rejected")
	}
	var result struct {
		Error  string `json:"error"`
		ErrID  string `json:"errid"`
		Result *bool  `json:"result"`
	}
	if json.Unmarshal(raw, &result) == nil && (result.Error != "" || result.ErrID != "" || (result.Result != nil && !*result.Result)) {
		detail := sanitizeQueueError(result.Error)
		if detail == "" {
			detail = sanitizeQueueError(result.ErrID)
		}
		if detail != "" {
			return nil, fmt.Errorf("deemix: submission failed: %s", detail)
		}
		return nil, errors.New("deemix: submission failed; check CantStream/FLAC availability")
	}
	interval := c.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	lastPath := ""
	var lastSize int64
	var lastMod time.Time
	stable := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, ext := range []string{".flac", ".mp3"} {
			path := filepath.Join(c.DownloadDir, t.ID+ext)
			st, e := os.Stat(path)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return nil, e
			}
			if st.Size() == 0 {
				// A preallocated download name is not a completed audio file.
				stable = 0
				continue
			}
			if lastPath == path && lastSize == st.Size() && lastMod.Equal(st.ModTime()) {
				stable++
			} else {
				stable = 0
				lastPath = path
				lastSize = st.Size()
				lastMod = st.ModTime()
			}
			if stable < 2 {
				break
			}
			p, e := c.SafePath(path)
			if e != nil {
				return nil, e
			}
			q, e := Inspect(p)
			if e != nil {
				return nil, e
			}
			if !q.Lossless && !c.lossyAllowed() {
				return nil, errors.New("deemix: MP3 fallback needs explicit --allow-320k")
			}
			if progress != nil {
				progress(QueueStatus{ID: t.ID, State: "completed", Progress: 1, Path: p})
			}
			return &Download{Path: p, Quality: q}, nil
		}
		if progress != nil {
			progress(QueueStatus{ID: t.ID, State: "downloading"})
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
