// Package joglogo is the Jog Logo Studio helper: 600px media-profile logos.
// Existing artwork is preserved for disable/restore.
package joglogo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

type feature struct {
	state  string
	path   string
	config map[string]any
	// library resolves a selected volume to its Engine Library (tests replace it).
	library func(volume any) (string, error)
}

// New returns the Jog Logo Studio feature.
func New(env modkit.Env) (modkit.Feature, error) {
	return newFeature(env.State)
}

func newFeature(state string) (*feature, error) {
	f := &feature{state: state, path: filepath.Join(state, "logo.png"),
		config: map[string]any{"sync": false, "volumes": []any{}, "enabled": false}, library: Library}
	cfg := filepath.Join(state, "config.json")
	if _, err := os.Stat(cfg); err == nil {
		var saved map[string]any
		if err := modkit.LoadJSON(cfg, &saved); err != nil {
			return nil, err
		}
		for k, v := range saved {
			f.config[k] = v
		}
	}
	return f, nil
}

func (f *feature) Status() (any, error) {
	out := map[string]any{}
	for k, v := range f.config {
		out[k] = v
	}
	st, err := os.Stat(f.path)
	out["ready"] = err == nil && st.Mode().IsRegular()
	out["preview"] = (&url.URL{Scheme: "file", Path: f.path}).String()
	out["note"] = "Reload the media profile on each player after changing a logo."
	return out, nil
}

// Library returns <volume>/Engine Library for a media volume below /media or /mnt.
func Library(volume any) (string, error) {
	s, ok := volume.(string)
	if !ok {
		return "", modkit.Bad("choose a media volume under /media or /mnt")
	}
	path, err := filepath.EvalSymlinks(s)
	if err != nil {
		return "", modkit.AsBad(err)
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", modkit.AsBad(err)
	}
	st, err := os.Stat(path)
	inside := false
	for _, root := range []string{"/media", "/mnt"} {
		if path != root && modkit.Within(path, root) {
			inside = true
		}
	}
	if err != nil || !st.IsDir() || !inside {
		return "", modkit.Bad("choose a media volume under /media or /mnt")
	}
	library := filepath.Join(path, "Engine Library")
	if lst, err := os.Lstat(library); err != nil || !lst.IsDir() {
		return "", modkit.Bad("media must already contain an Engine Library directory")
	}
	return library, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isSymlink(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// copyFile copies data; keep also copies mode and times (shutil.copy2).
func copyFile(src, dst string, keep bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if keep {
		st, err := os.Stat(src)
		if err != nil {
			return err
		}
		_ = os.Chmod(dst, st.Mode().Perm())
		_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
	}
	return nil
}

func (f *feature) Action(req map[string]any) (any, error) {
	action, err := modkit.Field(req, "action")
	if err != nil {
		return nil, err
	}
	switch action {
	case "crop":
		srcv, err := modkit.Field(req, "source")
		if err != nil {
			return nil, err
		}
		source, err := modkit.MediaFile(srcv, f.state)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(f.state, 0o755); err != nil {
			return nil, err
		}
		temp := filepath.Join(f.state, "logo.new.png")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", source,
			"-vf", "scale=600:600:force_original_aspect_ratio=increase,crop=600:600",
			"-frames:v", "1", "-update", "1", temp)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ffmpeg failed: %v: %s", err, bytes.TrimSpace(out))
		}
		header := make([]byte, 24)
		fh, err := os.Open(temp)
		if err != nil {
			return nil, err
		}
		n, _ := io.ReadFull(fh, header)
		fh.Close()
		if n < 24 || !bytes.Equal(header[:8], []byte("\x89PNG\r\n\x1a\n")) ||
			binary.BigEndian.Uint32(header[16:20]) != 600 || binary.BigEndian.Uint32(header[20:24]) != 600 {
			return nil, modkit.Bad("image conversion did not produce a 600x600 PNG")
		}
		if err := os.Rename(temp, f.path); err != nil {
			return nil, err
		}
	case "apply":
		return f.apply(req)
	default:
		return nil, modkit.Bad("unknown action")
	}
	return f.Status()
}

func (f *feature) apply(req map[string]any) (any, error) {
	for _, k := range []string{"volumes", "sync", "enabled"} {
		if _, err := modkit.Field(req, k); err != nil {
			return nil, err
		}
	}
	volumes, ok := req["volumes"].([]any)
	if !ok || len(volumes) < 1 || len(volumes) > 16 {
		return nil, modkit.Bad("select 1..16 volume paths")
	}
	sync, ok1 := req["sync"].(bool)
	enabled, ok2 := req["enabled"].(bool)
	if !ok1 || !ok2 {
		return nil, modkit.Bad("sync and enabled must be boolean")
	}
	if st, err := os.Stat(f.path); enabled && (err != nil || !st.Mode().IsRegular()) {
		return nil, modkit.Bad("crop an image first")
	}
	selected := volumes
	if !sync {
		selected = volumes[:1]
	}
	var libraries []string
	seen := map[string]bool{}
	for _, v := range selected {
		library, err := f.library(v)
		if err != nil {
			return nil, err
		}
		if !seen[library] {
			seen[library] = true
			libraries = append(libraries, library)
		}
	}
	// Validate every selected target before changing any target.
	for _, library := range libraries {
		for _, name := range []string{"logo.png", ".enginex-logo-original.png", ".enginex-logo-owned.json", ".enginex-logo-new.png"} {
			if isSymlink(filepath.Join(library, name)) {
				return nil, modkit.Bad("refusing a symlinked logo or backup")
			}
		}
	}
	for _, library := range libraries {
		target := filepath.Join(library, "logo.png")
		backup := filepath.Join(library, ".enginex-logo-original.png")
		marker := filepath.Join(library, ".enginex-logo-owned.json")
		if enabled {
			if !exists(marker) {
				if exists(backup) {
					return nil, modkit.Bad("unowned logo backup already exists; resolve it before applying")
				}
				had := exists(target)
				if had {
					if err := copyFile(target, backup, true); err != nil {
						return nil, err
					}
				}
				if err := modkit.SaveJSON(marker, map[string]any{"hadOriginal": had}); err != nil {
					return nil, err
				}
			}
			temp := filepath.Join(library, ".enginex-logo-new.png")
			if err := copyFile(f.path, temp, false); err != nil {
				return nil, err
			}
			if err := os.Rename(temp, target); err != nil {
				return nil, err
			}
		} else if exists(marker) {
			var owned map[string]any
			if err := modkit.LoadJSON(marker, &owned); err != nil {
				return nil, err
			}
			had, ok := owned["hadOriginal"]
			if !ok {
				return nil, modkit.Bad("'hadOriginal'")
			}
			if b, _ := had.(bool); b {
				if st, err := os.Stat(backup); err != nil || !st.Mode().IsRegular() {
					return nil, modkit.Bad("original logo backup is missing; refusing to remove current logo")
				}
				if err := os.Rename(backup, target); err != nil {
					return nil, err
				}
			} else if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			if err := os.Remove(marker); err != nil {
				return nil, err
			}
		}
	}
	f.config = map[string]any{"sync": sync, "volumes": volumes, "enabled": enabled}
	if err := modkit.SaveJSON(filepath.Join(f.state, "config.json"), f.config); err != nil {
		return nil, err
	}
	return f.Status()
}

func (f *feature) Close() {}
