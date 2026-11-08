package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// Public Spotify playlists and albums pasted as links need no login: the embed
// page carries the track list. They are fetched once and kept in LinksPath.

var (
	spLinkRe     = regexp.MustCompile(`(?:open\.spotify\.com/(?:intl-[a-z-]+/)?(?:embed/)?|spotify:)(playlist|album)[/:]([0-9A-Za-z]{22})`)
	spNextDataRe = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__"[^>]*>(.*?)</script>`)
)

type spLinked struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Tracks []Track `json:"tracks"`
}

// ParseSpotifyLinks returns the playlist/album references (kind, id) found in text.
func ParseSpotifyLinks(text string) [][2]string {
	var out [][2]string
	seen := map[string]bool{}
	for _, m := range spLinkRe.FindAllStringSubmatch(text, -1) {
		if !seen[m[2]] {
			seen[m[2]] = true
			out = append(out, [2]string{m[1], m[2]})
		}
	}
	return out
}

func (s *Spotify) linked() []spLinked {
	if s.LinksPath == "" {
		return nil
	}
	var v []spLinked
	if b, err := os.ReadFile(s.LinksPath); err == nil {
		json.Unmarshal(b, &v)
	}
	return v
}

// HasLinks reports whether any public playlist link is saved.
func (s *Spotify) HasLinks() bool { return len(s.linked()) > 0 }

func (s *Spotify) embedBase() string {
	if s.EmbedURL != "" {
		return strings.TrimRight(s.EmbedURL, "/")
	}
	return "https://open.spotify.com"
}

func (s *Spotify) fetchEmbed(ctx context.Context, kind, id string) (spLinked, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.embedBase()+"/embed/"+kind+"/"+id, nil)
	if err != nil {
		return spLinked{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client(s.HTTP).Do(req)
	if err != nil {
		return spLinked{}, errors.New("spotify: embed page unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return spLinked{}, StatusError(resp.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return spLinked{}, err
	}
	m := spNextDataRe.FindSubmatch(page)
	if m == nil {
		return spLinked{}, errors.New("spotify: embed data not found")
	}
	var d struct {
		Props struct {
			PageProps struct {
				State struct {
					Data struct {
						Entity struct {
							Name      string
							TrackList []struct {
								URI      string
								Title    string
								Subtitle string
								Duration int64
							}
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(m[1], &d); err != nil {
		return spLinked{}, errors.New("spotify: invalid embed data")
	}
	e := d.Props.PageProps.State.Data.Entity
	out := spLinked{ID: id, Name: e.Name}
	for _, t := range e.TrackList {
		tid, ok := strings.CutPrefix(t.URI, "spotify:track:")
		if !ok || tid == "" {
			continue
		}
		// Subtitle is "A, B" with a non-breaking space after the comma on some pages.
		artist := strings.ReplaceAll(t.Subtitle, " ", " ")
		var artists []string
		for _, a := range strings.Split(artist, ",") {
			if a = strings.TrimSpace(a); a != "" {
				artists = append(artists, a)
			}
		}
		out.Tracks = append(out.Tracks, Track{ID: tid, Title: t.Title, Artist: strings.Join(artists, ", "), Artists: artists, DurationMS: t.Duration, Provider: "spotify"})
	}
	if out.Name == "" || len(out.Tracks) == 0 {
		return spLinked{}, errors.New("spotify: playlist is empty or private")
	}
	return out, nil
}

// SetLinks replaces the saved public playlists with those named in text and
// returns how many were read. A link that cannot be read fails the whole call.
func (s *Spotify) SetLinks(ctx context.Context, text string) (int, error) {
	var all []spLinked
	for _, l := range ParseSpotifyLinks(text) {
		v, err := s.fetchEmbed(ctx, l[0], l[1])
		if err != nil {
			return 0, err
		}
		all = append(all, v)
	}
	if len(all) == 0 {
		return 0, errors.New("spotify: no playlist link found")
	}
	if s.LinksPath == "" {
		return 0, errors.New("spotify: no place to save links")
	}
	return len(all), SaveJSON(s.LinksPath, all)
}

func (s *Spotify) linkedPlaylists() []Playlist {
	var out []Playlist
	for _, v := range s.linked() {
		out = append(out, Playlist{v.ID, v.Name, len(v.Tracks)})
	}
	return out
}

func (s *Spotify) linkedTracks(id string) ([]Track, bool) {
	for _, v := range s.linked() {
		if v.ID == id {
			return v.Tracks, true
		}
	}
	return nil, false
}
