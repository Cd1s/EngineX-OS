package deemixclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func TestDownloadSurfacesQueueErrorDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(QueueStatus{
			ID:    "job-arl",
			State: "error",
			Error: "deezer: ARL invalid or expired",
		})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, PollInterval: time.Millisecond}
	_, err := c.Download(context.Background(), providers.Track{ID: "42", Provider: "deezer"}, nil)
	if err == nil || !strings.Contains(err.Error(), "ARL invalid or expired") {
		t.Fatalf("expected queue error detail to be preserved, got: %v", err)
	}

	leakSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(QueueStatus{
			ID:    "job-url",
			State: "error",
			Error: "cdn failure on https://cdns-proxy.dzcdn.net/mobile/1/secret-token",
		})
	}))
	defer leakSrv.Close()
	_, err = (&Client{BaseURL: leakSrv.URL, PollInterval: time.Millisecond}).Download(context.Background(), providers.Track{ID: "42", Provider: "deezer"}, nil)
	if err == nil || strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "cdn failure") {
		t.Fatalf("expected URL redaction with preserved error summary, got: %v", err)
	}

	dirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"result":false,"error":"ARL cookie rejected"}`))
	}))
	defer dirSrv.Close()
	_, err = (&Client{Mode: "directory", BaseURL: dirSrv.URL, DownloadDir: t.TempDir(), PollInterval: time.Millisecond}).Download(
		context.Background(), providers.Track{ID: "42", Provider: "deezer"}, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "ARL cookie rejected") {
		t.Fatalf("expected directory submission error detail, got: %v", err)
	}
}
