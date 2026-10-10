package porter

import (
	"math"
	"os"
	"runtime"
	"time"
)

// Status is the STATUS topic value. Optional fields are nil where the
// machine cannot report them.
type Status struct {
	Host          string    `json:"host"`
	OS            string    `json:"os"`
	UptimeS       int64     `json:"uptime_s"`
	Load          float64   `json:"load"`
	MemPct        int       `json:"mem_pct"`
	DiskPct       int       `json:"disk_pct"`
	TempC         *float64  `json:"temp_c,omitempty"`
	BatteryPct    *int      `json:"battery_pct,omitempty"`
	CommsVersion  string    `json:"comms_version"`
	AgentsRunning int       `json:"agents_running"`
	At            time.Time `json:"at"`
}

// CollectStatus samples this machine. disk is a path on the volume to
// report (the comms data dir). Failed probes leave zero or omitted values.
func CollectStatus(host, version, disk string, running int) Status {
	if host == "" {
		host, _ = os.Hostname()
	}
	s := Status{Host: host, OS: runtime.GOOS, CommsVersion: version, AgentsRunning: running, At: time.Now().UTC().Truncate(time.Second)}
	collect(&s, disk)
	s.Load = math.Round(s.Load*100) / 100
	return s
}

func pct(used, total float64) int {
	if total <= 0 {
		return 0
	}
	return int(math.Round(used / total * 100))
}
