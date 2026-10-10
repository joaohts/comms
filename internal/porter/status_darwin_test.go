package porter

import "testing"

func TestDarwinParsers(t *testing.T) {
	out := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:  1000.\nPages active:  5000.\nPages inactive:  1000.\nPages speculative:  0.\nPages purgeable: 0.\n"
	if got := vmStatPct(out, 16384*10000); got != 80 {
		t.Fatal(got)
	}
	if v, ok := batteryPct("Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t80%; discharging"); !ok || v != 80 {
		t.Fatal(v, ok)
	}
	if _, ok := batteryPct("Now drawing from 'AC Power'"); ok {
		t.Fatal("desktop reported a battery")
	}
}
