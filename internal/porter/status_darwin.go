package porter

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func collect(s *Status, disk string) {
	if tv, err := unix.SysctlTimeval("kern.boottime"); err == nil {
		s.UptimeS = time.Now().Unix() - tv.Sec
	}
	if raw, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(raw) >= 24 {
		// struct loadavg { fixpt_t ldavg[3]; long fscale; }
		ld := float64(uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24)
		var scale uint64
		for i := 0; i < 8; i++ {
			scale |= uint64(raw[16+i]) << (8 * i)
		}
		if scale > 0 {
			s.Load = ld / float64(scale)
		}
	}
	if total, err := unix.SysctlUint64("hw.memsize"); err == nil {
		if out := run("vm_stat"); out != "" {
			s.MemPct = vmStatPct(out, float64(total))
		}
	}
	var fs unix.Statfs_t
	if unix.Statfs(disk, &fs) == nil {
		total := float64(fs.Blocks) * float64(fs.Bsize)
		free := float64(fs.Bavail) * float64(fs.Bsize)
		s.DiskPct = pct(total-free, total)
	}
	if v, ok := batteryPct(run("pmset", "-g", "batt")); ok {
		s.BatteryPct = &v
	}
}

func run(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

var vmLine = regexp.MustCompile(`^(.+?):\s+(\d+)\.?$`)
var pageSize = regexp.MustCompile(`page size of (\d+) bytes`)

// vmStatPct counts free, inactive, speculative and purgeable pages as
// available, approximating Activity Monitor's memory used.
func vmStatPct(out string, total float64) int {
	page := 4096.0
	if m := pageSize.FindStringSubmatch(out); m != nil {
		page, _ = strconv.ParseFloat(m[1], 64)
	}
	var avail float64
	for _, line := range strings.Split(out, "\n") {
		m := vmLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		switch m[1] {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			v, _ := strconv.ParseFloat(m[2], 64)
			avail += v * page
		}
	}
	return pct(total-avail, total)
}

var battPct = regexp.MustCompile(`(\d+)%`)

func batteryPct(out string) (int, bool) {
	if !strings.Contains(out, "InternalBattery") {
		return 0, false
	}
	m := battPct.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	v, err := strconv.Atoi(m[1])
	return v, err == nil
}
