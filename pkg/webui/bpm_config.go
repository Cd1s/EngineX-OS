package webui

import (
	"errors"
	"net/http"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// The optional BPM service keys can be replaced but are never read back out.

// Tests may commit another writer before the atomic read/modify/write begins.
var testBPMConfigPostHook func()

func (s *Server) bpmConfigGet(w http.ResponseWriter, r *http.Request) {
	var v providers.BPMConfig
	if s.cfg.BPM != nil {
		v = s.cfg.BPM.Config()
	}
	respond(w, map[string]bool{"soundcloudSet": v.SoundCloudToken != "" || (v.SoundCloudClientID != "" && v.SoundCloudSecret != ""), "getSongBpmSet": v.GetSongBPMKey != ""})
}

func (s *Server) bpmConfigPost(w http.ResponseWriter, r *http.Request) {
	var in providers.BPMConfig
	if !input(w, r, &in) {
		return
	}
	if s.cfg.BPM == nil {
		fail(w, 503, errors.New("BPM lookup is off"))
		return
	}
	if testBPMConfigPostHook != nil {
		testBPMConfigPostHook()
	}
	err := s.cfg.BPM.UpdateConfig(func(v *providers.BPMConfig) {
		if in.SoundCloudClientID != "" {
			v.SoundCloudClientID = in.SoundCloudClientID
		}
		if in.SoundCloudSecret != "" {
			v.SoundCloudSecret = in.SoundCloudSecret
		}
		if in.SoundCloudToken != "" {
			v.SoundCloudToken = in.SoundCloudToken
		}
		if in.GetSongBPMKey != "" {
			v.GetSongBPMKey = in.GetSongBPMKey
		}
	})
	if err != nil {
		fail(w, 500, errors.New("could not save the BPM settings"))
		return
	}
	s.bpmConfigGet(w, r)
}
