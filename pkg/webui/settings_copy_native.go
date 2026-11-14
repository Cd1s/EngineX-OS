package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
)

// Names and scalar tags come from Engine 5.3's native user.profile exporter.
// PlayerColor*, Name and Product are deliberately absent.
var copyNativeBools = []string{
	"CensorBehavior", "ColorizeKey", "CueToLoopIn", "DisplayFileName",
	"IntegrateDemoContent", "IntegrateSamplerDemoContent", "LinkBeatJumpLoopSize",
	"MaintainPlayState", "MotorizedPlatterTorque", "NeedleLock", "OnAirMode",
	"PadLock", "PausedHotCueBehaviour", "PausedSavedLoopBehavior", "PlatterMode",
	"RemoteStartStop", "SavedLoopBehaviour", "ShowStemsList", "StemFxBehavior",
	"TimeDisplayMode", "TurntableMode",
}

var copyNativeEnums = []string{
	"BPMRange", "BpmTolerance", "CollectionBrowseBehavior", "DefaultBeatJump",
	"DefaultLoopSize", "DefaultPlayBackPosition", "DefaultSpeedRange", "KeyAdjust",
	"KeyCompatibility", "KeyDisplayMode", "KeySyncMode", "LockPlayingDeck",
	"LoopOutMode", "NudgeSensitivity", "PitchBendAction", "PitchBendBehavior",
	"QuantizeToGrid", "SyncButtonAction", "SyncIndicator", "SyncMode",
	"TrackOverviewStyle", "TrackSpeed", "WaveformStyle",
}

func copyNativeKeys() []string {
	keys := append([]string{"TrackEndWarningTime"}, copyNativeBools...)
	keys = append(keys, copyNativeEnums...)
	sort.Strings(keys)
	return keys
}

func copyNativeAllowed(key string) bool {
	return key == "TrackEndWarningTime" || slices.Contains(copyNativeBools, key) || slices.Contains(copyNativeEnums, key)
}

type copyNativeScalar struct {
	Type   int      `json:"type"`
	State  *bool    `json:"state,omitempty"`
	String *string  `json:"string,omitempty"`
	Value  *float64 `json:"value,omitempty"`
}

func validateCopyNative(key string, raw json.RawMessage) error {
	if !copyNativeAllowed(key) {
		return fmt.Errorf("native setting is not transferable: %s", key)
	}
	var v copyNativeScalar
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return fmt.Errorf("invalid native setting: %s", key)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("invalid native setting: %s", key)
	}
	valid := false
	switch {
	case slices.Contains(copyNativeBools, key):
		valid = v.Type == 1 && v.State != nil && v.String == nil && v.Value == nil
	case slices.Contains(copyNativeEnums, key):
		valid = v.Type == 4 && v.String != nil && len(*v.String) <= 128 && !strings.ContainsAny(*v.String, "\x00\r\n") && v.State == nil && v.Value == nil
	case key == "TrackEndWarningTime":
		// The native 5.3 Track End Warning translator spans 0..90 seconds.
		valid = v.Type == 0 && v.Value != nil && !math.IsNaN(*v.Value) && !math.IsInf(*v.Value, 0) && *v.Value >= 0 && *v.Value <= 90 && v.State == nil && v.String == nil
	}
	if !valid {
		return fmt.Errorf("invalid native setting: %s", key)
	}
	return nil
}

type copyNativeUser struct {
	Order    []string                   `json:"childorder"`
	Children map[string]json.RawMessage `json:"children"`
}

type copyNativeProfile struct {
	Data struct {
		User copyNativeUser `json:"User"`
	} `json:"data"`
}

func copyNativeExport(profile []byte) (map[string]json.RawMessage, error) {
	if len(profile) > 1<<20 {
		return nil, errors.New("native profile exceeds 1 MiB")
	}
	var p copyNativeProfile
	if err := json.Unmarshal(profile, &p); err != nil || p.Data.User.Children == nil {
		return nil, errors.New("invalid native profile")
	}
	values := map[string]json.RawMessage{}
	for key, raw := range p.Data.User.Children {
		if !copyNativeAllowed(key) {
			continue
		}
		if err := validateCopyNative(key, raw); err != nil {
			return nil, err
		}
		values[key] = raw
	}
	return values, nil
}

// entries contains the receiver's live native translator entries. Missing keys
// are unsupported on that receiver, including model-gated controls. The sender
// never gets to define which values the receiver accepts.
func copyNativeImport(values map[string]json.RawMessage, entries map[string][]string) ([]byte, []string, error) {
	var p copyNativeProfile
	p.Data.User.Children = map[string]json.RawMessage{}
	var skipped []string
	for key, raw := range values {
		if err := validateCopyNative(key, raw); err != nil {
			return nil, nil, err
		}
		allowed, supported := entries[key]
		if !supported {
			skipped = append(skipped, key)
			continue
		}
		if slices.Contains(copyNativeEnums, key) {
			var v copyNativeScalar
			_ = json.Unmarshal(raw, &v)
			if !slices.Contains(allowed, *v.String) {
				skipped = append(skipped, key)
				continue
			}
		}
		p.Data.User.Children[key] = raw
		p.Data.User.Order = append(p.Data.User.Order, key)
	}
	sort.Strings(p.Data.User.Order)
	sort.Strings(skipped)
	b, err := json.MarshalIndent(p, "", "    ")
	return b, skipped, err
}
