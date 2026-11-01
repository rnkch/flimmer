package sysinfo

import (
	"slices"
	"testing"
)

func TestThrottleFlags(t *testing.T) {
	for _, c := range []struct {
		raw          string
		now, earlier []ThrottleFlag
		has          bool
	}{
		{"0x0", nil, nil, true},
		{"0x80000", nil, []ThrottleFlag{SoftTempLimit}, true},            // was hot, fine now
		{"0x80008", []ThrottleFlag{SoftTempLimit}, nil, true},            // hot now: not repeated as "earlier"
		{"0x50005", []ThrottleFlag{UnderVoltage, Throttling}, nil, true}, // weak power supply right now
		{"0x10000", nil, []ThrottleFlag{UnderVoltage}, true},             // undervoltage since boot
		{"0xe0000", nil, []ThrottleFlag{FreqCapped, Throttling, SoftTempLimit}, true},
		{"", nil, nil, false}, // no vcgencmd (not a Pi)
	} {
		in := Info{Throttled: c.raw}
		if got := in.ThrottleNow(); !slices.Equal(got, c.now) {
			t.Errorf("%s now: %v, want %v", c.raw, got, c.now)
		}
		if got := in.ThrottleEarlier(); !slices.Equal(got, c.earlier) {
			t.Errorf("%s earlier: %v, want %v", c.raw, got, c.earlier)
		}
		if in.HasThrottle() != c.has {
			t.Errorf("%s has: %v", c.raw, in.HasThrottle())
		}
	}
}

func TestThrottleQuestions(t *testing.T) {
	for _, c := range []struct {
		raw                                  string
		uvNow, uvEarlier, slowNow, warmEarly bool
	}{
		{"0x0", false, false, false, false},
		{"0x80000", false, false, false, true}, // a Pi after installs: warm earlier, fine now
		{"0x80008", false, false, true, false}, // warm right now
		{"0x50005", true, false, false, false}, // weak supply now explains the throttling
		{"0x10000", false, true, false, false}, // weak supply earlier
		{"0x90000", false, true, false, true},  // both earlier
		{"", false, false, false, false},       // not a Pi
	} {
		in := Info{Throttled: c.raw}
		got := [4]bool{in.UnderVoltageNow(), in.UnderVoltageEarlier(), in.SlowedNow(), in.WarmEarlier()}
		if want := [4]bool{c.uvNow, c.uvEarlier, c.slowNow, c.warmEarly}; got != want {
			t.Errorf("%s: got %v, want %v", c.raw, got, want)
		}
	}
}
