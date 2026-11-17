package providers

import (
	"context"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/Cd1s/EngineX-OS/internal/smbclient"
)

func (p *SelfHostedProvider) smbConnect(ctx context.Context) (*smbclient.Client, error) {
	// NAS uses the same resolver/address policy as HTTP; configured filesystem
	// shares also work on direct-cable link-local networks.
	if transport, ok := p.httpClient().Transport.(*http.Transport); ok {
		return smbclient.Dial(ctx, p.cfg.URL, p.cfg.Username, p.cfg.Password, transport.DialContext)
	}
	return smbclient.Dial(ctx, p.cfg.URL, p.cfg.Username, p.cfg.Password, nil)
}

func (p *SelfHostedProvider) smbReadDir(ctx context.Context, dir string) ([]os.FileInfo, error) {
	rel, err := smbclient.Relative(dir)
	if err != nil {
		return nil, err
	}
	c, err := p.smbConnect(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.Share.ReadDir(path.Join(c.Root, rel))
}

func (p *SelfHostedProvider) smbTrack(dir string, info os.FileInfo) Track {
	return Track{ID: path.Join(dir, info.Name()), Title: strings.TrimSuffix(info.Name(), path.Ext(info.Name())), Album: path.Clean(dir), Provider: "smb"}
}

func (p *SelfHostedProvider) smbPlaylists(ctx context.Context) ([]Playlist, error) {
	entries, err := p.smbReadDir(ctx, ".")
	if err != nil {
		return nil, err
	}
	lists := []Playlist{{ID: "root", Title: "Root"}}
	for _, e := range entries {
		if e.IsDir() {
			lists = append(lists, Playlist{ID: "./" + e.Name(), Title: e.Name()})
		} else if webdavAudioExts[strings.ToLower(path.Ext(e.Name()))] {
			lists[0].Count++
			p.cacheWebDAVTrack(p.smbTrack(".", e))
		}
	}
	return lists, nil
}

func (p *SelfHostedProvider) smbPlaylistTracks(ctx context.Context, dir string) ([]Track, error) {
	if dir == "root" || dir == "" {
		dir = "."
	}
	entries, err := p.smbReadDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	var tracks []Track
	for _, e := range entries {
		if !e.IsDir() && webdavAudioExts[strings.ToLower(path.Ext(e.Name()))] {
			t := p.smbTrack(dir, e)
			p.cacheWebDAVTrack(t)
			tracks = append(tracks, t)
		}
	}
	return tracks, nil
}

func (p *SelfHostedProvider) smbSearch(ctx context.Context, query string) ([]Track, error) {
	p.mu.RLock()
	empty := len(p.webdavCache) == 0
	p.mu.RUnlock()
	if empty {
		if _, err := p.smbPlaylistTracks(ctx, "root"); err != nil {
			return nil, err
		}
	}
	return p.filenameSearch(query), nil
}

func (p *SelfHostedProvider) smbDownloadTrack(ctx context.Context, id, destDir string) (string, error) {
	rel, err := smbclient.Relative(id)
	if err != nil {
		return "", err
	}
	c, err := p.smbConnect(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close()
	f, err := c.Share.Open(path.Join(c.Root, rel))
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	u, _ := smbclient.Parse(p.cfg.URL)
	// A truncated remote file must not be published as a complete download.
	return saveStreamToFile(destDir, nasCacheFilename(u, u.Path+"\x00"+rel, path.Ext(rel)), &smbDownload{f, info.Size()})
}

type smbDownload struct {
	io.Reader
	remaining int64
}

func (r *smbDownload) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n, err := r.Reader.Read(p[:min(int64(len(p)), r.remaining)])
	r.remaining -= int64(n)
	if err == io.EOF && r.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
