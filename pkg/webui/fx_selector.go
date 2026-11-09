package webui

// Transient commands to Engine's UI hook. These are deliberately outside the
// saved preferences: a new Engine process always starts with stock effects.
import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *Server) fxSelectorReadyFile() string {
	if s.cfg.FXSelectorReadyFile != "" {
		return s.cfg.FXSelectorReadyFile
	}
	return "/run/enginex-fx-selector.ready"
}

func (s *Server) fxSelectorAvailable() bool {
	i, err := os.Stat(s.fxSelectorReadyFile())
	if err != nil || time.Since(i.ModTime()) >= 2*time.Second {
		return false
	}
	b, err := os.ReadFile(s.fxSelectorReadyFile())
	return err == nil && strings.TrimSpace(string(b)) == "armhf-px-v2 JC11"
}

// Queue ordinary cross-unit overlaps without holding the saved-settings lock.
// Bound the wait so a stalled hook cannot leave arbitrarily late selections.
func (s *Server) lockFXSelector(ctx context.Context) bool {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return false
		}
		if s.fxMu.TryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-tick.C:
		}
	}
}

func (s *Server) fxSelectorUpdate(w http.ResponseWriter, r *http.Request, q url.Values) {
	u, ue := strconv.Atoi(q.Get("fxUnit"))
	m, me := strconv.Atoi(q.Get("fxMode"))
	if ue != nil || me != nil || u < 1 || u > 6 || m < 0 || m > 5 {
		fail(w, http.StatusBadRequest, errors.New("invalid FX unit or mode"))
		return
	}
	if !s.lockFXSelector(r.Context()) {
		fail(w, http.StatusConflict, errors.New("FX selection already pending"))
		return
	}
	defer s.fxMu.Unlock()
	if !s.fxSelectorAvailable() {
		fail(w, http.StatusServiceUnavailable, errors.New("FX selector hook unavailable"))
		return
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		fail(w, http.StatusInternalServerError, errors.New("cannot create FX handshake"))
		return
	}
	// Distinct exponents and unit bits prevent cross-unit challenge matches.
	a := uint32(0x35800000) | (binary.LittleEndian.Uint32(nonce[:4]) & 0x007ffff8) | uint32(u-1)
	btoken := uint32(0x36000000) | (binary.LittleEndian.Uint32(nonce[4:]) & 0x007ffff8) | uint32(u-1)
	command := fmt.Sprintf("%d %d %d %d %d %d\n", u, m, time.Now().UnixNano(), a, btoken, time.Now().Add(750*time.Millisecond).UnixMilli())
	dir := s.uiDir()
	f, err := os.CreateTemp(dir, ".fx-selector-*")
	if err == nil {
		defer os.Remove(f.Name())
		_, err = f.WriteString(command)
		if ce := f.Close(); err == nil {
			err = ce
		}
		if err == nil {
			err = os.Rename(f.Name(), filepath.Join(dir, "fx-selector.request"))
		}
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, errors.New("cannot notify FX selector hook"))
		return
	}
	acked := false
	defer func() {
		if !acked {
			_ = os.Remove(filepath.Join(dir, "fx-selector.request"))
		}
	}()
	// Type may only be written once the hook has published the route. A stale
	// acknowledgement from a previous selection cannot satisfy this barrier.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		b, _ := os.ReadFile(filepath.Join(dir, "fx-selector.ack"))
		fields := strings.Fields(string(b))
		if len(fields) == 7 && strings.Join(fields[:6], " ") == strings.TrimSpace(command) && (fields[6] == "0" || fields[6] == "1") {
			acked = true
			respond(w, map[string]any{"fxSelectorReady": true, "fxBind": m > 0 && fields[6] == "0", "fxChallengeA": math.Float32frombits(a), "fxChallengeB": math.Float32frombits(btoken)})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			fail(w, http.StatusGatewayTimeout, errors.New("FX selector hook did not acknowledge"))
			return
		case <-tick.C:
		}
	}
}
