// Package deemixclient obtains audio files: from an existing local download
// service, or from plain HTTP addresses handed to it. It contains no media
// decryption, DRM code, or service credentials.
package deemixclient

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

var ErrNoMatch = errors.New("deemix: no unambiguous recording within 2 seconds")
var brackets = regexp.MustCompile(`\([^)]*\)|\[[^]]*\]|（[^）]*）|【[^】]*】`)
var versions = regexp.MustCompile(`(?i)\b(club|radio|extended|original|remix|mix|edit|live|acoustic|instrumental|dub|remaster|remastered|sped|slowed)\b`)

// "Original Mix" is how many stores name the default studio version of a track.
var originalMix = regexp.MustCompile(`(?i)\boriginal\s+(mix|version)\b|[(\[（【]\s*original\s*[)\]）】]`)
var remasterTag = regexp.MustCompile(`(?i)\bremaster(ed)?\b`)
var alternateRecording = regexp.MustCompile(`(?i)\b(live|acoustic|instrumental|dub|remix|sped|slowed)\b`)

func words(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 0xff01 && r <= 0xff5e {
			r -= 0xfee0
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// CleanTitle drops Explicit, Live and bracketed Chinese translations for
// catalog search. Matching separately retains recording-version qualifiers.
func CleanTitle(s string) string {
	original := s
	s = brackets.ReplaceAllStringFunc(s, func(v string) string {
		w := words(v)
		if w == "explicit" || w == "live" {
			return " "
		}
		han, latin := false, false
		for _, r := range v {
			han = han || unicode.Is(unicode.Han, r)
			latin = latin || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		}
		if han && !latin {
			return " "
		}
		return v
	})
	clean := words(s)
	if clean == "" {
		return words(original)
	}
	return clean
}
func version(s string) string {
	s = originalMix.ReplaceAllString(s, " ")
	v := versions.FindAllString(strings.ToLower(s), -1)
	sort.Strings(v)
	return strings.Join(v, " ")
}

func isrcVersionTokens(s string) map[string]bool {
	s = remasterTag.ReplaceAllString(s, " ")
	out := map[string]bool{}
	for _, tok := range strings.Fields(version(s)) {
		out[tok] = true
	}
	return out
}

func isrcVersionCompatible(a, b string) bool {
	ta, tb := isrcVersionTokens(a), isrcVersionTokens(b)
	aSubB, bSubA := true, true
	for k := range ta {
		if !tb[k] {
			aSubB = false
			if alternateRecording.MatchString(k) {
				return false
			}
		}
	}
	for k := range tb {
		if !ta[k] {
			bSubA = false
			if alternateRecording.MatchString(k) {
				return false
			}
		}
	}
	if aSubB && bSubA {
		return true
	}
	// Do not conflate an explicit "(Original Mix)" with a different named mix ("Club Mix"),
	// even if a distributor mis-assigned the same ISRC.
	if (aSubB && originalMix.MatchString(a)) || (bSubA && originalMix.MatchString(b)) {
		return false
	}
	return aSubB || bSubA
}
func isrc(s string) string { return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s)) }
func artist(s string) string {
	parts := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || r == ';' || r == '&' || r == '、' })
	for i := range parts {
		parts[i] = words(parts[i])
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func artistIdentity(t providers.Track) string {
	if len(t.Artists) == 0 {
		return artist(t.Artist)
	}
	parts := []string{}
	seen := map[string]bool{}
	for _, v := range t.Artists {
		w := words(v)
		if w != "" && !seen[w] {
			parts = append(parts, w)
			seen[w] = true
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}
func durationOK(a, b int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return a > 0 && b > 0 && d <= 2000
}
func Match(source providers.Track, candidates []providers.Track) (providers.Track, error) {
	exact := []providers.Track{}
	fallback := []providers.Track{}
	seen := map[string]bool{}
	for _, c := range candidates {
		if c.ID == "" || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		if source.ISRC != "" && c.ISRC != "" && isrc(source.ISRC) == isrc(c.ISRC) {
			if (source.DurationMS == 0 || c.DurationMS == 0 || durationOK(source.DurationMS, c.DurationMS)) && isrcVersionCompatible(source.Title, c.Title) {
				exact = append(exact, c)
			}
			continue
		}
		if source.ISRC != "" && c.ISRC != "" {
			continue
		}
		if SearchTitle(source.Title) != "" && artistIdentity(source) != "" && durationOK(source.DurationMS, c.DurationMS) && SearchTitle(source.Title) == SearchTitle(c.Title) && artistIdentity(source) == artistIdentity(c) && version(source.Title) == version(c.Title) {
			fallback = append(fallback, c)
		}
	}
	if len(exact) > 0 {
		sort.Slice(exact, func(i, j int) bool { return exact[i].ID < exact[j].ID })
		return exact[0], nil
	}
	if len(fallback) == 1 {
		return fallback[0], nil
	}
	return providers.Track{}, ErrNoMatch
}

// SearchTitle is the title with "Original Mix" removed, for cross-catalog lookups.
func SearchTitle(title string) string {
	if s := CleanTitle(originalMix.ReplaceAllString(title, " ")); s != "" {
		return s
	}
	return CleanTitle(title)
}

func artistSet(t providers.Track) map[string]bool {
	set := map[string]bool{}
	for _, a := range strings.Split(artistIdentity(t), "|") {
		if a != "" {
			set[a] = true
		}
	}
	return set
}

// MatchNearby finds the same recording in a catalog that shares no IDs with the
// source: same title and version, a shared artist, and a duration within
// tolerance. Catalogs hold duplicate uploads, so the closest duration wins.
func MatchNearby(source providers.Track, candidates []providers.Track, toleranceMS int64) (providers.Track, error) {
	title := SearchTitle(source.Title)
	want := artistSet(source)
	var best providers.Track
	bestGap := int64(-1)
	for _, c := range candidates {
		gap := source.DurationMS - c.DurationMS
		if gap < 0 {
			gap = -gap
		}
		if c.ID == "" || title == "" || source.DurationMS <= 0 || c.DurationMS <= 0 || gap > toleranceMS || SearchTitle(c.Title) != title || version(SearchTitle(c.Title)) != version(title) {
			continue
		}
		shared := false
		for a := range artistSet(c) {
			shared = shared || want[a]
		}
		if shared && (bestGap < 0 || gap < bestGap || (gap == bestGap && c.ID < best.ID)) {
			best, bestGap = c, gap
		}
	}
	if bestGap < 0 {
		return providers.Track{}, ErrNoMatch
	}
	return best, nil
}

type Catalog interface {
	ByISRC(context.Context, string) (providers.Track, error)
	Search(context.Context, string) ([]providers.Track, error)
}

func Resolve(ctx context.Context, c Catalog, source providers.Track) (providers.Track, error) {
	if source.Provider == "deezer" && source.ID != "" {
		// A track read directly from Deezer already has its catalog identity.
		if id, err := strconv.ParseUint(source.ID, 10, 64); err == nil && id > 0 {
			return source, nil
		}
	}
	if source.ISRC != "" {
		if t, err := c.ByISRC(ctx, source.ISRC); err == nil {
			if m, e := Match(source, []providers.Track{t}); e == nil {
				return m, nil
			}
		}
	}
	searchTitle := SearchTitle(source.Title)
	if searchTitle == "" || artistIdentity(source) == "" {
		return providers.Track{}, ErrNoMatch
	}
	primary := source.Artist
	if len(source.Artists) > 0 {
		primary = source.Artists[0]
	} else {
		primary = strings.TrimSpace(strings.Split(primary, ",")[0])
	}
	tracks, err := c.Search(ctx, fmt.Sprintf("artist:%q track:%q", primary, searchTitle))
	if err == nil && len(tracks) == 0 {
		// Deezer's field search can come back empty for tracks its plain search
		// finds (2026-11: even "Daft Punk" "One More Time"). Match below still
		// requires the same title, version, artist and duration.
		tracks, err = c.Search(ctx, primary+" "+searchTitle)
	}
	if err != nil {
		return providers.Track{}, err
	}
	// Search responses often omit contributors. Fetch only plausible candidates
	// before comparing the complete collaboration, not just the main artist.
	if full, ok := c.(interface {
		ByID(context.Context, string) (providers.Track, error)
	}); ok && strings.Contains(artistIdentity(source), "|") {
		for i, t := range tracks {
			if durationOK(source.DurationMS, t.DurationMS) && SearchTitle(t.Title) == searchTitle && version(source.Title) == version(t.Title) {
				if detailed, e := full.ByID(ctx, t.ID); e == nil {
					tracks[i] = detailed
				}
			}
		}
	}
	return Match(source, tracks)
}
