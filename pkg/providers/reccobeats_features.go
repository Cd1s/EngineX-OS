package providers

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// AudioFeatures represents the audio feature metrics returned by ReccoBeats.
type AudioFeatures struct {
	ID               string  `json:"id"`
	Energy           float64 `json:"energy"`
	Danceability     float64 `json:"danceability"`
	Valence          float64 `json:"valence"`
	Acousticness     float64 `json:"acousticness"`
	Instrumentalness float64 `json:"instrumentalness"`
	Speechiness      float64 `json:"speechiness"`
	Liveness         float64 `json:"liveness"`
	Tempo            float64 `json:"tempo"`
}

// FetchReccoBeatsAudioFeatures retrieves the audio features for a given track ID via GET /v1/track/{id}/audio-features.
func FetchReccoBeatsAudioFeatures(ctx context.Context, client *http.Client, baseURL, trackID string) (AudioFeatures, error) {
	u := strings.TrimRight(baseURL, "/")
	if u == "" {
		u = DefaultReccoURL
	}
	endpoint := u + "/v1/track/" + url.PathEscape(trackID) + "/audio-features"
	var feat AudioFeatures
	if err := JSON(ctx, client, http.MethodGet, endpoint, nil, nil, &feat); err != nil {
		return AudioFeatures{}, err
	}
	return feat, nil
}

// ExtractReccoBeatsAudioFeatures extracts audio features from an audio stream via POST /v1/analysis/audio-features.
func ExtractReccoBeatsAudioFeatures(ctx context.Context, client *http.Client, baseURL string, audio io.Reader) (AudioFeatures, error) {
	u := strings.TrimRight(baseURL, "/")
	if u == "" {
		u = DefaultReccoURL
	}
	endpoint := u + "/v1/analysis/audio-features"
	hdr := http.Header{"Content-Type": []string{"application/octet-stream"}}
	var feat AudioFeatures
	if err := JSON(ctx, client, http.MethodPost, endpoint, audio, hdr, &feat); err != nil {
		return AudioFeatures{}, err
	}
	return feat, nil
}

// FilterByAudioFeatures filters items by energy, danceability, and valence min/max bounds.
func FilterByAudioFeatures(items []AudioFeatures, minEnergy, maxEnergy, minDance, maxDance, minValence, maxValence float64) []AudioFeatures {
	res := make([]AudioFeatures, 0, len(items))
	for _, it := range items {
		if minEnergy > 0 && it.Energy < minEnergy {
			continue
		}
		if maxEnergy > 0 && it.Energy > maxEnergy {
			continue
		}
		if minDance > 0 && it.Danceability < minDance {
			continue
		}
		if maxDance > 0 && it.Danceability > maxDance {
			continue
		}
		if minValence > 0 && it.Valence < minValence {
			continue
		}
		if maxValence > 0 && it.Valence > maxValence {
			continue
		}
		res = append(res, it)
	}
	return res
}
