//go:build linux

package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// heldReplaced reports whether this process still holds open the file that a rename replaced
// at path: the kernel names it "<path> (deleted)".
func heldReplaced(t *testing.T, path string) bool {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		if l, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && l == path+" (deleted)" {
			return true
		}
	}
	return false
}

// #321, #253 review 3: on FAT a rename frees the replaced file's clusters at once, while the
// entry naming the new ones reaches the stick only with the inode's writeback. Replaying every
// write of a pass on a FAT32 volume, a pull in that window left the ANLZ files and the
// OneLibrary export unreadable (EIO); an unlink has the same two writes. Each file a pass
// replaces or removes must stay open, so its clusters stay allocated, until the directory (and
// the new file) are synced, and be closed after.
func TestIssue321ReplacedFileStaysAllocatedUntilTheNewEntryIsSynced(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatal(err)
	}
	injectDeckCueEdit(t, stick)
	held := map[string]bool{}
	var leaked []string
	restore := bridge.SetWriteBackCutForTest(func(step string) {
		dst, ok := strings.CutPrefix(step, "renamed ")
		if !ok {
			dst, ok = strings.CutPrefix(step, "removed ") // the pass's mark, at its end
		}
		if !ok || strings.Contains(dst, ".enginex-backup") {
			return
		}
		held[filepath.Base(dst)] = heldReplaced(t, dst)
	})
	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true})
	restore()
	if err != nil || res.FilesUpdated == 0 || !res.OneLibraryUpdated {
		t.Fatalf("pass: %+v %v", res, err)
	}
	for _, name := range []string{"ANLZ0000.DAT", "ANLZ0000.EXT", "exportLibrary.db", ".rb-bridge-writeback"} {
		if !held[name] {
			t.Errorf("%s: the replaced or removed file was not held open until its directory was synced (seen %v)", name, held)
		}
	}
	for name := range held {
		for _, dir := range []string{filepath.Join(stick, "PIONEER", "USBANLZ", "P001"), filepath.Join(stick, "PIONEER", "rekordbox"), filepath.Join(stick, "Engine Library", "Database2")} {
			if heldReplaced(t, filepath.Join(dir, name)) {
				leaked = append(leaked, name)
			}
		}
	}
	if len(leaked) != 0 {
		t.Fatalf("still held after the pass: %v", leaked)
	}
}
