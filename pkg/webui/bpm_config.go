package webui

import (
	"errors"
	"net/http"

	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
)

// The optional BPM service keys can be replaced but are never read back out.

func (s *Server) bpmConfigGet(w http.ResponseWriter, r *http.Request) {
	var v providers.BPMConfig
	if s.cfg.BPM != nil {
		v = s.cfg.BPM.Config()
	}
	respond(w, map[string]bool{"soundcloudSet": v.SoundCloudClientID != "" || v.SoundCloudToken != "", "getSongBpmSet": v.GetSongBPMKey != ""})
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
	v := s.cfg.BPM.Config()
	if in.SoundCloudClientID != "" {
		v.SoundCloudClientID = in.SoundCloudClientID
	}
	if in.SoundCloudToken != "" {
		v.SoundCloudToken = in.SoundCloudToken
	}
	if in.GetSongBPMKey != "" {
		v.GetSongBPMKey = in.GetSongBPMKey
	}
	if err := s.cfg.BPM.SaveConfig(v); err != nil {
		fail(w, 500, errors.New("could not save the BPM settings"))
		return
	}
	s.bpmConfigGet(w, r)
}
