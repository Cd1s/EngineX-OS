package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchReccoBeatsAudioFeatures(t *testing.T) {
	expected := AudioFeatures{
		ID:               "rec-12345",
		Energy:           0.85,
		Danceability:     0.78,
		Valence:          0.65,
		Acousticness:     0.12,
		Instrumentalness: 0.05,
		Speechiness:      0.04,
		Liveness:         0.18,
		Tempo:            126.0,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/v1/track/rec-12345/audio-features" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(expected)
			return
		}
		if r.URL.Path == "/v1/track/nonexistent/audio-features" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		t.Errorf("unexpected path: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	ctx := context.Background()
	feat, err := FetchReccoBeatsAudioFeatures(ctx, srv.Client(), srv.URL, "rec-12345")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if feat != expected {
		t.Fatalf("got %+v, want %+v", feat, expected)
	}

	// Test 404
	_, err = FetchReccoBeatsAudioFeatures(ctx, srv.Client(), srv.URL, "nonexistent")
	if err == nil {
		t.Fatalf("expected error for nonexistent track, got nil")
	}
}

func TestExtractReccoBeatsAudioFeatures(t *testing.T) {
	expected := AudioFeatures{
		ID:               "analyzed-999",
		Energy:           0.92,
		Danceability:     0.81,
		Valence:          0.54,
		Acousticness:     0.02,
		Instrumentalness: 0.88,
		Speechiness:      0.03,
		Liveness:         0.11,
		Tempo:            130.0,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/v1/analysis/audio-features" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("unexpected content-type: %s", ct)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "fake-audio-payload" {
			t.Errorf("unexpected body: %s, err: %v", string(body), err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(expected)
	}))
	defer srv.Close()

	ctx := context.Background()
	audio := bytes.NewBufferString("fake-audio-payload")
	feat, err := ExtractReccoBeatsAudioFeatures(ctx, srv.Client(), srv.URL, audio)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if feat != expected {
		t.Fatalf("got %+v, want %+v", feat, expected)
	}

	// Test server error
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()

	_, err = ExtractReccoBeatsAudioFeatures(ctx, errSrv.Client(), errSrv.URL, bytes.NewBufferString("test"))
	if err == nil {
		t.Fatalf("expected error on 500, got nil")
	}
}

func TestFilterByAudioFeatures(t *testing.T) {
	items := []AudioFeatures{
		{ID: "t1", Energy: 0.2, Danceability: 0.3, Valence: 0.4},
		{ID: "t2", Energy: 0.5, Danceability: 0.6, Valence: 0.7},
		{ID: "t3", Energy: 0.8, Danceability: 0.9, Valence: 0.2},
		{ID: "t4", Energy: 0.9, Danceability: 0.4, Valence: 0.8},
	}

	// Filter by Energy [0.4, 0.85]
	f1 := FilterByAudioFeatures(items, 0.4, 0.85, 0, 0, 0, 0)
	if len(f1) != 2 || f1[0].ID != "t2" || f1[1].ID != "t3" {
		t.Errorf("filter 1 failed: %+v", f1)
	}

	// Filter by Danceability [0.5, 0.95]
	f2 := FilterByAudioFeatures(items, 0, 0, 0.5, 0.95, 0, 0)
	if len(f2) != 2 || f2[0].ID != "t2" || f2[1].ID != "t3" {
		t.Errorf("filter 2 failed: %+v", f2)
	}

	// Filter by Valence [0.3, 0.75]
	f3 := FilterByAudioFeatures(items, 0, 0, 0, 0, 0.3, 0.75)
	if len(f3) != 2 || f3[0].ID != "t1" || f3[1].ID != "t2" {
		t.Errorf("filter 3 failed: %+v", f3)
	}

	// Combined filter: Energy >= 0.4, Dance >= 0.5, Valence >= 0.5
	f4 := FilterByAudioFeatures(items, 0.4, 1.0, 0.5, 1.0, 0.5, 1.0)
	if len(f4) != 1 || f4[0].ID != "t2" {
		t.Errorf("filter 4 failed: %+v", f4)
	}

	// No filters applied
	f5 := FilterByAudioFeatures(items, 0, 0, 0, 0, 0, 0)
	if len(f5) != 4 {
		t.Errorf("filter 5 failed: want 4 items, got %d", len(f5))
	}
}
