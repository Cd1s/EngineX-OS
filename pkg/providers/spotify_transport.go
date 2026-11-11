package providers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
	authpb "github.com/devgianlu/go-librespot/proto/spotify"
)

var ErrSpotifyPairingBusy = errors.New("spotify: authentication is busy")
var ErrSpotifyAuthentication = errors.New("spotify: permanent authentication failure")

type spotifyAuthFailure struct {
	message   string
	cause     error
	permanent bool
}

func (e *spotifyAuthFailure) Error() string { return e.message }
func (e *spotifyAuthFailure) Unwrap() error { return e.cause }
func (e *spotifyAuthFailure) Is(target error) bool {
	return e.permanent && target == ErrSpotifyAuthentication
}

// v0.10.5 AP.Connect retries every error. Cancel its retry context as soon as it
// reports permanent credentials rejection, without ever logging server details.
type spotifyAPLogger struct {
	librespot.NullLogger
	cancel   context.CancelFunc
	mu       sync.Mutex
	rejected error
}

func (l *spotifyAPLogger) WithField(string, interface{}) librespot.Logger { return l }
func (l *spotifyAPLogger) WithError(err error) librespot.Logger {
	var rejected *ap.AccesspointLoginError
	if errors.As(err, &rejected) && rejected.Message.GetErrorCode() == authpb.ErrorCode_BadCredentials {
		l.mu.Lock()
		l.rejected = err
		l.mu.Unlock()
		l.cancel()
	}
	return l
}
func (l *spotifyAPLogger) rejection() error { l.mu.Lock(); defer l.mu.Unlock(); return l.rejected }

func (n *spotifyProtocol) watchCountry(ch <-chan ap.Packet) {
	go func() {
		defer n.countryOnce.Do(func() { close(n.countryReady) })
		for {
			select {
			case <-n.ap.Done():
				return
			case pkt, ok := <-ch:
				if !ok {
					return
				}
				if len(pkt.Payload) == 2 {
					country := string(pkt.Payload)
					n.country.Store(&country)
					n.countryOnce.Do(func() { close(n.countryReady) })
				}
			}
		}
	}()
}
func (n *spotifyProtocol) countryCode(ctx context.Context) string {
	if n.countryReady != nil {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-n.countryReady:
		case <-ctx.Done():
		case <-timer.C:
		}
	}
	if country := n.country.Load(); country != nil {
		return *country
	}
	return ""
}

// A caller's request context bounds the body; a slow transfer is not aborted by
// the JSON client's 30-second total timeout. Keep its transport and test hooks.
func spotifyMediaClient(c *http.Client) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	media := *c
	media.Timeout = 0
	transport := c.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		clone := t.Clone()
		clone.ResponseHeaderTimeout = 30 * time.Second
		media.Transport = clone
	}
	return &media
}
