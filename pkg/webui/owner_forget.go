package webui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

var testSoundCloudForgetHook func()

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
		// An account signed in through the relay keeps its tokens in the store it
		// shares with the app's token (soundcloud-token.json), not in the owner
		// file: they go from memory and from disk, the app's token stays.
		if err := s.soundCloudAuth.SetUser("", "", time.Time{}); err != nil {
			return err
		}
		var owner soundCloudOwner
		b, err := os.ReadFile(s.soundCloudOwnerPath())
		if err != nil {
			return nil // nothing saved
		}
		unreadable := json.Unmarshal(b, &owner) != nil
		// Linking with a pasted token copies it to the lookup keys, where
		// ownerSoundCloudProvider would find it again for the next session.
		if !unreadable && owner.Token != "" && s.cfg.BPM != nil {
			if keys := s.cfg.BPM.Config(); keys.SoundCloudToken == owner.Token {
				if testSoundCloudForgetHook != nil {
					testSoundCloudForgetHook()
				}
				if err := s.cfg.BPM.UpdateConfig(func(keys *providers.BPMConfig) {
					if keys.SoundCloudToken == owner.Token {
						keys.SoundCloudToken = ""
					}
				}); err != nil {
					return err
				}
			}
		}
		if unreadable || (owner.ClientID == "" && owner.Secret == "") {
			return remove(s.soundCloudOwnerPath())
		}
		owner.UserID, owner.Username, owner.Token = 0, "", ""
		return providers.SaveJSON(s.soundCloudOwnerPath(), owner)
	}
	return nil
}
