package webui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSettingsCopyNativeProfileExcludesIdentityAndSecrets(t *testing.T) {
	profile := []byte(`{"data":{"User":{"childorder":["PlayerColor4A","Name","Product","DefaultSpeedRange"],"children":{
"PlayerColor4A":{"color":"#123456","type":8},
"Name":{"string":"private deck name","type":4},
"Product":{"string":"JP07","type":4},
"PlayerNumber":{"value":1,"type":0},
"futureAccountSecret":{"string":"never-export-this","type":4},
"DefaultSpeedRange":{"string":"±20","type":4},
"DisplayFileName":{"state":false,"type":1},
"MotorizedPlatterTorque":{"state":true,"type":1}
}}}}`)
	values, err := copyNativeExport(profile)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(values)
	for _, key := range []string{"PlayerColor4A", "Name", "Product", "PlayerNumber", "futureAccountSecret"} {
		if _, found := values[key]; found {
			t.Errorf("native payload exported identity/secret key %q", key)
		}
	}
	for _, forbidden := range []string{"private deck", "never-export"} {
		if bytes.Contains(wire, []byte(forbidden)) {
			t.Errorf("native payload leaked %q: %s", forbidden, wire)
		}
	}
	// A normal player does not advertise the motor torque control.
	got, skipped, err := copyNativeImport(values, map[string][]string{"DefaultSpeedRange": {"±8", "±20"}, "DisplayFileName": nil})
	if err != nil || len(skipped) != 1 || skipped[0] != "MotorizedPlatterTorque" {
		t.Fatalf("native model skip: %s %v %v", got, skipped, err)
	}
	if bytes.Contains(got, []byte("PlayerColor")) || !bytes.Contains(got, []byte(`"state": false`)) {
		t.Fatalf("filtered native profile contains identity or loses false: %s", got)
	}
	if got, skipped, err := copyNativeImport(values, map[string][]string{"DefaultSpeedRange": {"±8"}}); err != nil || len(skipped) != 3 || bytes.Contains(got, []byte("DefaultSpeedRange")) {
		t.Fatalf("unsupported enum must be skipped: %s %v %v", got, skipped, err)
	}
}

func TestSettingsCopyNativeRejectsMalformedValues(t *testing.T) {
	for _, tc := range []struct{ key, raw string }{
		{"PlayerColor4A", `{"color":"#123456","type":8}`},
		{"DisplayFileName", `{"state":"false","type":1}`},
		{"DisplayFileName", `{"state":false,"type":4}`},
		{"DisplayFileName", `{"state":false,"type":1,"string":"secret"}`},
		{"DisplayFileName", `null`},
		{"DisplayFileName", `{"state":false,"type":1} {}`},
		{"DefaultSpeedRange", `{"string":null,"type":4}`},
		{"DefaultSpeedRange", `{"string":"` + strings.Repeat("a", 129) + `","type":4}`},
		{"TrackEndWarningTime", `{"value":-1,"type":0}`},
		{"TrackEndWarningTime", `{"value":1e100,"type":0}`},
	} {
		if err := validateCopyNative(tc.key, json.RawMessage(tc.raw)); err == nil {
			t.Errorf("accepted %s=%s", tc.key, tc.raw)
		}
	}
}
