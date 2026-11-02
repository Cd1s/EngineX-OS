package webui

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// UnblockRadios clears rfkill's soft block on Bluetooth and Wi-Fi. The kernel boots
// with rfkill.default_state=0. This runs from the writable partition, so it also
// works on a deck whose read-only root still has the older sc5000-debloat.
func UnblockRadios(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		kind, err := os.ReadFile(filepath.Join(dir, "type"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(kind)) {
		case "bluetooth", "wlan":
			if err := os.WriteFile(filepath.Join(dir, "soft"), []byte("0"), 0o644); err != nil {
				log.Printf("stream-sync: could not unblock %s: %v", e.Name(), err)
			}
		}
	}
}
