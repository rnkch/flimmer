// Package sysinfo reads host health figures. Values that a platform does not
// provide (e.g. CPU temperature on a Mac) are left at zero / empty.
package sysinfo

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type Info struct {
	DiskTotal uint64        `json:"disk_total"`
	DiskFree  uint64        `json:"disk_free"` // available to unprivileged users
	TempC     float64       `json:"temp_c"`
	Throttled string        `json:"throttled"` // raw vcgencmd value, e.g. "0x0"
	Load1     float64       `json:"load1"`
	MemAvail  uint64        `json:"mem_avail"`
	MemTotal  uint64        `json:"mem_total"`
	Uptime    time.Duration `json:"uptime"`
}

// DiskFree returns the bytes available to the service user on the
// filesystem holding path.
func DiskFree(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Bavail * bs, st.Blocks * bs, nil
}

func Read(ctx context.Context, dataDir string) Info {
	var in Info
	in.DiskFree, in.DiskTotal, _ = DiskFree(dataDir)
	if b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			in.TempC = v / 1000
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			in.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s, _ := strconv.ParseFloat(f[0], 64)
			in.Uptime = time.Duration(s) * time.Second
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			kb, _ := strconv.ParseUint(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				in.MemTotal = kb * 1024
			case "MemAvailable:":
				in.MemAvail = kb * 1024
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "vcgencmd", "get_throttled").Output(); err == nil {
		in.Throttled = strings.TrimPrefix(strings.TrimSpace(string(out)), "throttled=")
	}
	return in
}

// ThrottleFlag is one condition reported by vcgencmd get_throttled. The value
// holds each flag twice: in the low bits while it is happening now, and 16
// bits higher once it has happened since boot (raspberrypi.com/documentation,
// "vcgencmd get_throttled").
type ThrottleFlag uint32

const (
	UnderVoltage  ThrottleFlag = 1 << 0 // power supply too weak
	FreqCapped    ThrottleFlag = 1 << 1 // ARM frequency capped
	Throttling    ThrottleFlag = 1 << 2 // currently throttled
	SoftTempLimit ThrottleFlag = 1 << 3 // soft temperature limit (60 °C on a 3B+)
)

var throttleFlags = []ThrottleFlag{UnderVoltage, FreqCapped, Throttling, SoftTempLimit}

func (in Info) throttleBits() (uint32, bool) {
	v, err := strconv.ParseUint(strings.TrimPrefix(in.Throttled, "0x"), 16, 32)
	return uint32(v), err == nil
}

// HasThrottle reports whether the Pi's firmware gave a throttle value at all
// (not on other hosts).
func (in Info) HasThrottle() bool {
	_, ok := in.throttleBits()
	return ok
}

// ThrottleNow lists the conditions active right now.
func (in Info) ThrottleNow() []ThrottleFlag {
	v, _ := in.throttleBits()
	var out []ThrottleFlag
	for _, f := range throttleFlags {
		if v&uint32(f) != 0 {
			out = append(out, f)
		}
	}
	return out
}

// ThrottleEarlier lists the conditions that occurred since boot but are not
// active now.
func (in Info) ThrottleEarlier() []ThrottleFlag {
	v, _ := in.throttleBits()
	var out []ThrottleFlag
	for _, f := range throttleFlags {
		if v&(uint32(f)<<16) != 0 && v&uint32(f) == 0 {
			out = append(out, f)
		}
	}
	return out
}

// The questions a user actually has, answered from the flags. On a Pi 3B+ the
// clock is capped either for undervoltage or for heat; when the supply is too
// weak, that is the explanation, so SlowedNow only counts heat.

// UnderVoltageNow: the power supply is too weak right now.
func (in Info) UnderVoltageNow() bool { return slices.Contains(in.ThrottleNow(), UnderVoltage) }

// UnderVoltageEarlier: it was too weak since boot, but isn't now.
func (in Info) UnderVoltageEarlier() bool {
	return slices.Contains(in.ThrottleEarlier(), UnderVoltage)
}

// SlowedNow: running slower right now because it is warm.
func (in Info) SlowedNow() bool {
	return !in.UnderVoltageNow() && slices.ContainsFunc(in.ThrottleNow(), isHeat)
}

// WarmEarlier: was slowed down for heat since boot, but isn't now. Normal
// under load (transcoding); not worth showing to non-technical users.
func (in Info) WarmEarlier() bool { return slices.ContainsFunc(in.ThrottleEarlier(), isHeat) }

func isHeat(f ThrottleFlag) bool { return f != UnderVoltage }
