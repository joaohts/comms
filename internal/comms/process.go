package comms

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// ProcessStamp distinguishes a live harness process from a reused PID.
// An empty stamp means the caller cannot prove process identity; lease expiry
// then only marks it offline and explicit close/takeover is required.
func ProcessStamp(pid int) string {
	if pid <= 0 {
		return ""
	}
	if runtime.GOOS == "linux" {
		b, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if e != nil {
			return ""
		}
		text := string(b)
		i := strings.LastIndex(text, ") ")
		if i < 0 {
			return ""
		}
		fields := strings.Fields(text[i+2:])
		if len(fields) < 20 || fields[0] == "Z" {
			return ""
		}
		boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if e != nil {
			return ""
		}
		return strings.TrimSpace(string(boot)) + ":" + fields[19]
	}
	// macOS has no /proc. ps start time plus the actual process executable keeps
	// the identity check independent of the Agent Monitor GUI.
	out, e := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=,comm=").Output()
	if e != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func processMatches(pid int, stamp string) bool { return stamp != "" && ProcessStamp(pid) == stamp }

func confirmedProcessEnded(pid int, stamp string) bool {
	if pid <= 0 || stamp == "" {
		return false
	}
	if e := unix.Kill(pid, 0); errors.Is(e, unix.ESRCH) {
		return true
	}
	if runtime.GOOS == "linux" {
		b, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if e == nil {
			text := string(b)
			i := strings.LastIndex(text, ") ")
			if i >= 0 && strings.HasPrefix(text[i+2:], "Z ") {
				return true
			}
		}
	}
	current := ProcessStamp(pid)
	return current != "" && current != stamp
}
