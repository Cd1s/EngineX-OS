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

func TestObserveBoundsTheTableAndStampsLastSeen(t *testing.T) {
	d := NewDiscovery()
	for i := 0; i < maxDevices+10; i++ {
		d.Observe(Device{Address: "10.0.0.1", Number: byte(i)})
	}
	if len(d.devices) != maxDevices {
		t.Fatalf("%d devices", len(d.devices))
	}
	for _, v := range d.devices {
		if v.LastSeen.IsZero() {
			t.Fatal("zero LastSeen stored")
		}
	}
}
