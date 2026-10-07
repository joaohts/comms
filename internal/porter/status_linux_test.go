package porter

import "testing"

func TestMemPct(t *testing.T) {
	if got := memPct("MemTotal:  1000 kB\nMemFree: 100 kB\nMemAvailable:   400 kB\n"); got != 60 {
		t.Fatal(got)
	}
}
