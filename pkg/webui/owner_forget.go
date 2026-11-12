package webui

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// forgetOwner removes the saved login of an owner account that was signed out in
// the console. Only Spotify did this (its Logout); the other services kept their
// file, and the "signed out" mark is in memory, so the account was back after the
// next restart (BH2-A5-7). What is not the account stays: SoundCloud's app
// client ID and secret are settings of the deck.
func (s *Server) forgetOwner(provider string) error {
	if s.cfg.StateDir == "" {
		return nil
	}
	remove := func(path string) error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	switch provider {
	case "deezer":
		return remove(s.deezerOwnerPath())
	case "netease", "applemusic":
		return remove(filepath.Join(s.cfg.StateDir, provider+"-owner.json"))
	case "beatport":
		if s.beatport != nil {
			return s.beatport.Forget()
		}
		return remove(filepath.Join(s.cfg.StateDir, "beatport-owner.json"))
	case "soundcloud":
		var owner soundCloudOwner
		b, err := os.ReadFile(s.soundCloudOwnerPath())
		if err != nil {
			return nil // nothing saved
		}
		if json.Unmarshal(b, &owner) != nil || (owner.ClientID == "" && owner.Secret == "") {
			return remove(s.soundCloudOwnerPath())
		}
		owner.UserID, owner.Username, owner.Token = 0, "", ""
		return providers.SaveJSON(s.soundCloudOwnerPath(), owner)
	}
	return nil
}
