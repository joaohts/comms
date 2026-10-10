package porter

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func collect(s *Status, disk string) {
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			s.UptimeS = int64(v)
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s.Load, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		s.MemPct = memPct(string(b))
	}
	var fs unix.Statfs_t
	if unix.Statfs(disk, &fs) == nil {
		total := float64(fs.Blocks) * float64(fs.Bsize)
		free := float64(fs.Bavail) * float64(fs.Bsize)
		s.DiskPct = pct(total-free, total)
	}
	if b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			t := math.Round(v/100) / 10
			s.TempC = &t
		}
	}
	if m, _ := filepath.Glob("/sys/class/power_supply/BAT*/capacity"); len(m) > 0 {
		if b, err := os.ReadFile(m[0]); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				s.BatteryPct = &v
			}
		}
	}
}

// memPct reads /proc/meminfo: used = MemTotal - MemAvailable.
func memPct(meminfo string) int {
	var total, avail float64
	for _, line := range strings.Split(meminfo, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(f[1], 64)
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	return pct(total-avail, total)
}
