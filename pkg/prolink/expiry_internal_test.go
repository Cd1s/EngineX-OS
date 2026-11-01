package prolink

import (
	"testing"
	"time"
)

func TestObservePrunesStorage(t *testing.T) {
	d := NewDiscovery()
	d.Observe(Device{Number: 1, LastSeen: time.Now().Add(-time.Minute)})
	d.Observe(Device{Number: 2, LastSeen: time.Now()})
	if len(d.devices) != 1 {
		t.Fatal("expired device retained", d.devices)
	}
	d.Devices(time.Now().Add(time.Minute))
	if len(d.devices) != 0 {
		t.Fatal("Devices retained expired storage")
	}
}
