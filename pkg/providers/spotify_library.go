package providers

import (
	"context"
	"errors"
	"io"
	"math/big"
	"strings"

	librespot "github.com/devgianlu/go-librespot"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	extpb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/devgianlu/go-librespot/spclient"
)

const (
	spotifyMetadataBatchSize = 100
	maxNativeLibraryTracks   = 5000
)

type spotifyNativeLibrary interface {
	LibraryPlaylists(context.Context) ([]Playlist, error)
	LibraryTracks(context.Context, string) ([]Track, error)
}

type spotifyBatchedMetadataFunc func(context.Context, *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error)

func (s *Spotify) nativeLibraryFallback(err error) bool {
	var status StatusError
	return err != nil && errors.As(err, &status) && (status == 401 || status == 403 || status == 429) && s.HasStoredCredentials()
}
func (s *Spotify) nativePlaylists(ctx context.Context) (out []Playlist, err error) {
	err = s.withNative(ctx, func(n spotifyNative) error {
		lib, ok := n.(spotifyNativeLibrary)
		if !ok {
			return errors.New("spotify: native library unavailable")
		}
		var err error
		out, err = lib.LibraryPlaylists(ctx)
		return err
	})
	return
}
func (s *Spotify) nativeTracks(ctx context.Context, id string) (out []Track, err error) {
	if id != SpotifyLikedID && !spotifyTrackIDRe.MatchString(id) {
		return nil, errors.New("spotify: invalid playlist ID")
	}
	err = s.withNative(ctx, func(n spotifyNative) error {
		lib, ok := n.(spotifyNativeLibrary)
		if !ok {
			return errors.New("spotify: native library unavailable")
		}
		var err error
		out, err = lib.LibraryTracks(ctx, id)
		return err
	})
	return
}
func (n *spotifyProtocol) LibraryPlaylists(ctx context.Context) ([]Playlist, error) {
	all := []Playlist{{ID: SpotifyLikedID, Title: "Liked Songs"}}
	for offset := 0; offset < 100000; {
		list, err := n.sp.Rootlist(ctx, n.username, offset, 100)
		if err != nil {
			return nil, err
		}
		content := list.GetContents()
		items := content.GetItems()
		meta := content.GetMetaItems()
		for i, item := range items {
			id, err := spotifyLibraryID(item.GetUri(), librespot.SpotifyIdTypePlaylist)
			if err != nil {
				continue
			}
			p := Playlist{ID: id.Base62(), Title: id.Base62()}
			if i < len(meta) {
				p.Title = meta[i].GetAttributes().GetName()
				p.Count = int(meta[i].GetLength())
			}
			all = append(all, p)
		}
		offset += len(items)
		if !content.GetTruncated() || len(items) == 0 {
			return all, nil
		}
	}
	return nil, errors.New("spotify: native rootlist exceeds limit")
}
func spotifyLibraryID(uri string, typ librespot.SpotifyIdType) (*librespot.SpotifyId, error) {
	prefix := "spotify:" + string(typ) + ":"
	raw := strings.TrimPrefix(uri, prefix)
	v, ok := new(big.Int).SetString(raw, 62)
	if !strings.HasPrefix(uri, prefix) || len(raw) < 21 || len(raw) > 22 || !ok || v.BitLen() > 128 {
		return nil, errors.New("spotify: invalid library URI")
	}
	return librespot.SpotifyIdFromUri(uri)
}

type spotifyContextPageResolver interface {
	Page(context.Context, int) ([]*connectpb.ContextTrack, error)
}

func (n *spotifyProtocol) LibraryTracks(ctx context.Context, id string) ([]Track, error) {
	if id != SpotifyLikedID && !spotifyTrackIDRe.MatchString(id) {
		return nil, errors.New("spotify: invalid playlist ID")
	}
	uri := "spotify:playlist:" + id
	if id == SpotifyLikedID {
		uri = "spotify:user:" + n.username + ":collection"
	}
	// ContextResolve handles collection contexts and routes playlists through the
	// playlist service; ContextResolver follows all subsequent context pages.
	resolved, err := n.sp.ContextResolve(ctx, uri)
	if err != nil {
		return nil, err
	}
	pages, err := spclient.NewContextResolver(ctx, &librespot.NullLogger{}, n.sp, resolved)
	if err != nil {
		return nil, err
	}
	return collectSpotifyContextTracks(ctx, pages, n.sp.ExtendedMetadata)
}

func collectSpotifyContextTracks(ctx context.Context, pages spotifyContextPageResolver, request spotifyBatchedMetadataFunc) ([]Track, error) {
	var ids []librespot.SpotifyId
	for page := 0; ; page++ {
		tracks, err := pages.Page(ctx, page)
		if err == io.EOF {
			break
		}
		if err != nil {
			if err.Error() == "invalid empty page without url" {
				if page >= 2000 {
					return nil, errors.New("spotify: native playlist exceeds maximum page limit")
				}
				continue
			}
			return nil, err
		}
		if page >= 2000 {
			return nil, errors.New("spotify: native playlist exceeds maximum page limit")
		}
		for _, track := range tracks {
			spotID, err := spotifyLibraryID(track.GetUri(), librespot.SpotifyIdTypeTrack)
			if err != nil {
				continue
			}
			ids = append(ids, *spotID)
			if len(ids) > maxNativeLibraryTracks {
				return nil, errors.New("spotify: native playlist exceeds maximum track limit")
			}
		}
	}
	return resolveSpotifyLibraryTracks(ctx, ids, request)
}

func resolveSpotifyLibraryTracks(ctx context.Context, ids []librespot.SpotifyId, request spotifyBatchedMetadataFunc) ([]Track, error) {
	if len(ids) > maxNativeLibraryTracks {
		ids = ids[:maxNativeLibraryTracks]
	}
	all := make([]Track, 0, len(ids))
	for start := 0; start < len(ids); start += spotifyMetadataBatchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := ids[start:min(start+spotifyMetadataBatchSize, len(ids))]
		req := &extpb.BatchedEntityRequest{EntityRequest: make([]*extpb.EntityRequest, len(chunk))}
		for i, spotID := range chunk {
			req.EntityRequest[i] = &extpb.EntityRequest{
				EntityUri: spotID.Uri(),
				Query:     []*extpb.ExtensionQuery{{ExtensionKind: extpb.ExtensionKind_TRACK_V4}},
			}
		}
		resp, err := request(ctx, req)
		if err != nil {
			return nil, err
		}
		byURI := make(map[string]*metapb.Track, len(chunk))
		for _, item := range resp.GetExtendedMetadata() {
			if item.GetExtensionKind() != extpb.ExtensionKind_TRACK_V4 {
				continue
			}
			for _, extData := range item.GetExtensionData() {
				if extData.GetHeader().GetStatusCode() != 200 || extData.GetExtensionData() == nil {
					continue
				}
				var m metapb.Track
				if extData.GetExtensionData().UnmarshalTo(&m) == nil {
					byURI[extData.GetEntityUri()] = &m
				}
			}
		}
		for _, spotID := range chunk {
			m := byURI[spotID.Uri()]
			if m == nil {
				continue
			}
			artists := make([]string, 0, len(m.Artist))
			for _, a := range m.Artist {
				artists = append(artists, a.GetName())
			}
			all = append(all, Track{
				ID:         spotID.Base62(),
				Title:      m.GetName(),
				Artist:     strings.Join(artists, ", "),
				Artists:    artists,
				Album:      m.GetAlbum().GetName(),
				DurationMS: int64(m.GetDuration()),
				Provider:   "spotify",
			})
		}
	}
	return all, nil
}
