package webui

import (
	"context"
	"io"
	"net/http"
	"os"

	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

const (
	maxPreviewBytes = 1 << 20
	// A shorter clip gives no usable tempo.
	minPreviewSeconds = 10
)

// PreviewAnalyzer estimates tempo and key from a short public preview clip, under
// the same resource guard as full analysis. Nothing is kept once it returns.
func PreviewAnalyzer(guard *resourceguard.Guard) func(context.Context, string) providers.BPMInfo {
	return func(ctx context.Context, address string) providers.BPMInfo {
		req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
		if err != nil {
			return providers.BPMInfo{}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return providers.BPMInfo{}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return providers.BPMInfo{}
		}
		f, err := os.CreateTemp("", "bpm-preview-*.mp3")
		if err != nil {
			return providers.BPMInfo{}
		}
		defer os.Remove(f.Name())
		_, err = io.Copy(f, io.LimitReader(resp.Body, maxPreviewBytes))
		if closeErr := f.Close(); err != nil || closeErr != nil {
			return providers.BPMInfo{}
		}
		var info providers.BPMInfo
		guard.Run(ctx, func(t *resourceguard.Throttle) error {
			_, res, err := analyzer.AnalyzeFile(f.Name(), analyzer.Options{Ticker: t})
			if err == nil && res.Duration >= minPreviewSeconds {
				info = providers.BPMInfo{BPM: res.BPM, Key: res.Key.Camelot}
			}
			return err
		})
		return info
	}
}
