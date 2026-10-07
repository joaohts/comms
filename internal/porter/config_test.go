package porter

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestConfigRoundTripAndShares(t *testing.T) {
	path := filepath.Join(t.TempDir(), "porter", "config.toml")
	c, err := LoadConfig(path)
	if err != nil || c.Exists || c.Approvals != ApprovalsAuto || c.AskTimeout != 2*time.Minute || !c.Shares(TopicAgents, "m_x") {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c.Approvers = []string{"m_phone"}
	c.Approvals = ApprovalsPhone
	c.AskTimeout = 90 * time.Second
	c.Share[TopicStatus] = []string{"m_pi"}
	if err = SaveConfig(path, c); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	got, err := LoadConfig(path)
	if err != nil || !got.Exists || !reflect.DeepEqual(got.Approvers, c.Approvers) || got.Approvals != ApprovalsPhone || got.AskTimeout != 90*time.Second || got.IdleThreshold != 2*time.Minute {
		t.Fatalf("reload: %+v %v\n%s", got, err, c.Encode())
	}
	if !got.Shares(TopicAgents, "m_any") || got.Shares(TopicStatus, "m_mac") || !got.Shares(TopicStatus, "m_pi") {
		t.Fatalf("share: %+v", got.Share)
	}
	if got.Shares(TopicTrusts, "m_pi") || !got.Shares(TopicTrusts, "m_phone") || got.Shares("location", "m_phone") {
		t.Fatal("trusts must go to approvers only; unlisted topics nowhere")
	}
}

func TestConfigParsesHandWrittenTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(`# mine
approvers = ["m_a", "m_b"]   # two phones
approvals = "terminal"
idle_threshold = "30s"
future = "ignored"

[share]
agents = "*"
status = "m_pi"
location = ["m_pi", "m_mac"]
`), 0o600)
	c, err := LoadConfig(path)
	if err != nil || len(c.Approvers) != 2 || c.Approvals != ApprovalsTerminal || c.IdleThreshold != 30*time.Second {
		t.Fatalf("%+v %v", c, err)
	}
	if !c.Shares(TopicAgents, "m_x") || !c.Shares(TopicStatus, "m_pi") || c.Shares(TopicStatus, "m_x") || !c.Shares("location", "m_mac") {
		t.Fatalf("share: %+v", c.Share)
	}
	for _, bad := range []string{`approvals = "maybe"`, `ask_timeout = "soon"`, `approvers = [m_a]`, "[other]\nx = \"y\"", `nonsense`} {
		os.WriteFile(path, []byte(bad), 0o600)
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestTrustLabelsAndStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trusts.json")
	err := UpdateJSON(path, func(ts *Trusts) (bool, error) {
		ts.Put(Trust{ReceiverID: "a_r", SenderID: "m_w:a_s", Scope: TrustSession, ApprovedAt: t0})
		ts.Put(Trust{ReceiverID: "a_r", SenderID: "m_w:a_s", Scope: TrustAlways, ApprovedAt: t0.Add(time.Second)})
		ts.Put(Trust{ReceiverID: "a_r", SenderID: "m_w:a_e", Scope: TrustSession, ApprovedAt: t0})
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := LoadJSON[Trusts](path)
	if err != nil || len(ts.Trusts) != 2 {
		t.Fatalf("pair must be replaced, not duplicated: %+v %v", ts, err)
	}
	if ts.Label("a_r", "m_w:a_s") != "trusted: always" || ts.Label("a_r", "m_w:a_e") != "trusted: session" || ts.Label("a_other", "m_w:a_s") != "untrusted" {
		t.Fatal("labels")
	}
	l := ts.List()
	if l[0].Scope != TrustAlways || l[0].Item().SenderID != "m_w:a_s" {
		t.Fatalf("order: %+v", l)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{2 * time.Minute: "2m", 90 * time.Second: "1m30s", time.Hour: "1h", 30 * time.Second: "30s"} {
		if got := shortDuration(d); got != want {
			t.Fatalf("%v → %s", d, got)
		}
	}
}

func TestCollectStatus(t *testing.T) {
	s := CollectStatus("box", "0.2.0", t.TempDir(), 3)
	if s.Host != "box" || s.OS == "" || s.CommsVersion != "0.2.0" || s.AgentsRunning != 3 || s.At.IsZero() {
		t.Fatalf("%+v", s)
	}
	if s.DiskPct <= 0 || s.DiskPct > 100 || s.MemPct < 0 || s.MemPct > 100 {
		t.Fatalf("implausible: %+v", s)
	}
}
