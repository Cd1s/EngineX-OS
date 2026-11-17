package wifi

import (
	"strings"
	"testing"
)

func TestGetPowerSaveMissingInterface(t *testing.T) {
	_, err := GetPowerSave("nonexistent_if0")
	if err == nil {
		t.Fatal("expected error for nonexistent interface, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent_if0") {
		t.Fatalf("expected error mentioning interface name, got: %v", err)
	}
}

func TestSetPowerSaveMissingInterface(t *testing.T) {
	err := SetPowerSave("nonexistent_if0", false)
	if err == nil {
		t.Fatal("expected error for nonexistent interface, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent_if0") {
		t.Fatalf("expected error mentioning interface name, got: %v", err)
	}
}

func TestAlign4(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 0},
		{1, 4},
		{2, 4},
		{3, 4},
		{4, 4},
		{5, 8},
		{8, 8},
		{11, 12},
		{12, 12},
	}
	for _, c := range cases {
		if got := align4(c.in); got != c.want {
			t.Errorf("align4(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
