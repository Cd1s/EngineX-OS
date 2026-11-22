package providers

import (
	"os"
	"strings"
	"testing"
)

// GO-2026-5051: go-smb2 v1.1.0 reads past the end of a malformed directory listing and panics.
// The listing call must come back as an error for that folder, not stop the program.
func TestSMBListingTurnsALibraryPanicIntoAnError(t *testing.T) {
	entries, err := smbListing(func() ([]os.FileInfo, error) {
		var short []byte
		_ = short[3] // what the library does with a short reply
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "malformed directory listing") {
		t.Fatalf("err = %v, want a malformed-listing error", err)
	}
	if entries != nil {
		t.Fatalf("entries = %v, want none", entries)
	}
}

func TestSMBListingPassesAGoodReplyThrough(t *testing.T) {
	want := []os.FileInfo{}
	got, err := smbListing(func() ([]os.FileInfo, error) { return want, nil })
	if err != nil || got == nil {
		t.Fatalf("got %v, %v; want the reply unchanged", got, err)
	}
	wantErr := os.ErrNotExist
	if _, err := smbListing(func() ([]os.FileInfo, error) { return nil, wantErr }); err != wantErr {
		t.Fatalf("err = %v, want the library's own error unchanged", err)
	}
}
