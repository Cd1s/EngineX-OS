package providers

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
	extpb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	filespb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata/audiofiles"
	metapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"google.golang.org/protobuf/proto"
)

type spotifyMetadataRequest func(context.Context, librespot.SpotifyId, extpb.ExtensionKind, proto.Message) error

func spotifyTrackRestricted(t *metapb.Track, country string) bool {
	if len(country) != 2 {
		return false
	}
	contains := func(list string) bool {
		for i := 0; i+1 < len(list); i += 2 {
			if strings.EqualFold(list[i:i+2], country) {
				return true
			}
		}
		return false
	}
	for _, restriction := range librespot.NewMediaFromTrack(t).Restriction() {
		switch r := restriction.CountryRestriction.(type) {
		case *metapb.Restriction_CountriesAllowed:
			return !contains(r.CountriesAllowed)
		case *metapb.Restriction_CountriesForbidden:
			return contains(r.CountriesForbidden)
		}
	}
	return false
}

// File extensions and GIDs must be resolved together: TRACK_V4 often has no files.
func resolveSpotifyMetadata(ctx context.Context, id librespot.SpotifyId, country string, request spotifyMetadataRequest) (*metapb.Track, error) {
	var root metapb.Track
	if err := request(ctx, id, extpb.ExtensionKind_TRACK_V4, &root); err != nil {
		return nil, err
	}
	if len(root.Gid) == 0 {
		root.Gid = id.Id()
	}
	var available bool
	for _, track := range append([]*metapb.Track{&root}, root.Alternative...) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if track == nil || len(track.Gid) != 16 || spotifyTrackRestricted(track, country) {
			continue
		}
		available = true
		trackID := librespot.NewMediaFromTrack(track).Id()
		var extension filespb.AudioFilesExtensionResponse
		err := request(ctx, trackID, extpb.ExtensionKind_AUDIO_FILES, &extension)
		files := track.File
		if err == nil && len(extension.Files) > 0 {
			files = nil
			for _, f := range extension.Files {
				if f != nil && f.File != nil {
					files = append(files, f.File)
				}
			}
		} else if err != nil && len(files) == 0 && isSpotifyTransportError(err) {
			return nil, err
		}
		candidate := &metapb.Track{Gid: track.Gid, File: files}
		if _, file := selectSpotifyFile(candidate); file != nil {
			out := proto.Clone(&root).(*metapb.Track)
			out.Gid = track.Gid
			out.File = files
			out.Alternative = nil
			return out, nil
		}
	}
	if !available {
		return nil, librespot.ErrMediaRestricted
	}
	return nil, errors.New("spotify: no playable Ogg Vorbis audio files")
}

func isSpotifyTransportError(err error) bool {
	var ne net.Error
	return errors.Is(err, ap.ErrAccesspointClosed) ||
		errors.As(err, &ne) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}
