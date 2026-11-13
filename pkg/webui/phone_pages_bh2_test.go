package webui

import (
	"strings"
	"testing"
)

// BH2-A5-18: the link that opens Spotify's device sign-in used class "button", which the
// phone stylesheet does not have (its buttons are button and a.b), so it showed as plain text.
func TestSpotifyDevicePageLinkIsAStyledButton(t *testing.T) {
	page := spotifyDevicePhonePage(0, "https://accounts.spotify.com/pair?code=AB12", "AB12")
	if !strings.Contains(page, `<a class="b" href="https://accounts.spotify.com/pair?code=AB12">`) {
		t.Fatalf("the sign-in link is not an a.b button: %s", page)
	}
	if strings.Contains(page, `class="button"`) {
		t.Fatalf("class \"button\" has no style on the phone pages: %s", page)
	}
}
