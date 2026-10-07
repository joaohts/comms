package porter

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

func mustApply(t *testing.T, s *State, e Event) *Agent {
	t.Helper()
	a, err := s.Apply(e)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLifecycleAccumulatesRunningAndWaiting(t *testing.T) {
	s := NewState()
	mustApply(t, s, Event{Agent: "a", Harness: "claude", Kind: KindStart, At: at(0), Project: "notes"})
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(10)})
	a := mustApply(t, s, Event{Agent: "a", Kind: KindNeeds, At: at(70), Needs: &Needs{Kind: "permission", Text: "Bash: ls"}})
	if a.Status != StatusNeedsYou || a.Needs == nil || a.Needs.Text != "Bash: ls" {
		t.Fatalf("needs not recorded: %+v", a)
	}
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(100)})
	a = mustApply(t, s, Event{Agent: "a", Kind: KindStop, At: at(130)})
	if a.Status != StatusDone || a.Needs != nil {
		t.Fatalf("unexpected final state: %+v", a)
	}
	if a.RunningMS != 90_000 || a.WaitingMS != 30_000 {
		t.Fatalf("running=%d waiting=%d", a.RunningMS, a.WaitingMS)
	}
	if !a.StartedAt.Equal(at(0)) || !a.StatusSince.Equal(at(130)) || !a.LastActiveAt.Equal(at(130)) {
		t.Fatalf("timestamps: %+v", a)
	}
	if a.Harness != "claude" || a.Project != "notes" {
		t.Fatalf("metadata lost: %+v", a)
	}
}

func TestUpdateKeepsStatusAndTimers(t *testing.T) {
	s := NewState()
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(0)})
	yes := true
	a := mustApply(t, s, Event{Agent: "a", Kind: KindUpdate, At: at(30), Summary: "doing x", Special: &yes})
	if a.Status != StatusRunning || !a.StatusSince.Equal(at(0)) || a.RunningMS != 0 {
		t.Fatalf("update changed status period: %+v", a)
	}
	if a.Summary != "doing x" || !a.Special {
		t.Fatalf("metadata not applied: %+v", a)
	}
}

func TestLateEventNeverSubtracts(t *testing.T) {
	s := NewState()
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(100)})
	a := mustApply(t, s, Event{Agent: "a", Kind: KindStop, At: at(50), Summary: "late"})
	if a.Status != StatusRunning || a.RunningMS != 0 || !a.StatusSince.Equal(at(100)) || !a.LastActiveAt.Equal(at(100)) {
		t.Fatalf("late event: %+v", a)
	}
	if a.Summary != "late" {
		t.Fatalf("late event: %+v", a)
	}
}

func TestEndBuriesAndBadInputRejected(t *testing.T) {
	s := NewState()
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(0)})
	mustApply(t, s, Event{Agent: "sub", Parent: "a", AgentType: "Explore", Kind: KindPrompt, At: at(0)})
	a := mustApply(t, s, Event{Agent: "a", Kind: KindEnd, At: at(10)})
	if a.Status != StatusEnded || a.EndReason != EndClosed || a.EndedAt == nil || !a.EndedAt.Equal(at(10)) || a.RunningMS != 10_000 {
		t.Fatalf("end did not bury agent: %+v", a)
	}
	if c := s.Agents["sub"]; c.Status != StatusEnded || c.Parent != "a" || c.AgentType != "Explore" {
		t.Fatalf("subagent outlived parent: %+v", c)
	}
	if a := mustApply(t, s, Event{Agent: "ghost", Kind: KindEnd}); a != nil || s.Agents["ghost"] != nil {
		t.Fatal("end of unknown agent created it")
	}
	// A resumed session leaves the graveyard.
	a = mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(20)})
	if a.Status != StatusRunning || a.EndedAt != nil || a.EndReason != "" {
		t.Fatalf("resume: %+v", a)
	}
	if _, err := s.Apply(Event{Agent: "", Kind: KindPrompt}); err == nil {
		t.Fatal("empty agent accepted")
	}
	if _, err := s.Apply(Event{Agent: "a", Kind: "bogus"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestListPutsNeedsYouFirst(t *testing.T) {
	s := NewState()
	mustApply(t, s, Event{Agent: "old-needs", Kind: KindNeeds, At: at(0)})
	mustApply(t, s, Event{Agent: "recent", Kind: KindPrompt, At: at(50)})
	mustApply(t, s, Event{Agent: "older", Kind: KindStop, At: at(10)})
	mustApply(t, s, Event{Agent: "dead", Kind: KindPrompt, At: at(60)})
	mustApply(t, s, Event{Agent: "dead", Kind: KindEnd, At: at(61)})
	l := s.List()
	if l[0].ID != "old-needs" || l[1].ID != "recent" || l[2].ID != "older" || l[3].ID != "dead" {
		t.Fatalf("order: %s %s %s %s", l[0].ID, l[1].ID, l[2].ID, l[3].ID)
	}
	if s.Running() != 1 {
		t.Fatalf("running=%d", s.Running())
	}
}

func TestSweepStaleAndPurge(t *testing.T) {
	s := NewState()
	mustApply(t, s, Event{Agent: "quiet", Kind: KindPrompt, At: at(0)})
	mustApply(t, s, Event{Agent: "busy", Kind: KindPrompt, At: at(0).Add(StaleAfter)})
	changed, purged := s.Sweep(at(0).Add(StaleAfter + time.Minute))
	q := s.Agents["quiet"]
	if !changed || len(purged) != 0 || q.Status != StatusEnded || q.EndReason != EndStale || s.Agents["busy"].Status != StatusRunning {
		t.Fatalf("stale sweep: %v %v %+v", changed, purged, q)
	}
	if changed, _ := s.Sweep(at(0).Add(StaleAfter + 2*time.Minute)); changed {
		t.Fatal("idempotent sweep reported a change")
	}
	_, purged = s.Sweep(q.EndedAt.Add(KeepEnded + time.Second))
	if len(purged) != 1 || purged[0] != "quiet" || s.Agents["quiet"] != nil {
		t.Fatalf("purge: %v", purged)
	}
}

func TestSpecialSticksToPersistentIdentity(t *testing.T) {
	s := NewState()
	a := mustApply(t, s, Event{Agent: "s1", Kind: KindStart, At: at(0)})
	s.SetIdentity(a, Identity{Alias: "joana", Address: "pi:joana", Persistent: true})
	s.SetSpecial(a, true)
	b := mustApply(t, s, Event{Agent: "s2", Kind: KindStart, At: at(1)})
	if !s.SetIdentity(b, Identity{Alias: "joana", Address: "pi:joana", Persistent: true}) || !b.Special {
		t.Fatal("special did not follow the persistent identity")
	}
	if s.SetIdentity(b, Identity{Alias: "joana", Address: "pi:joana", Persistent: true}) {
		t.Fatal("unchanged identity reported a change")
	}
	c := mustApply(t, s, Event{Agent: "s3", Kind: KindStart, At: at(2)})
	s.SetIdentity(c, Identity{Alias: "tmp", Address: "pi:tmp"})
	s.SetSpecial(c, true)
	d := mustApply(t, s, Event{Agent: "s4", Kind: KindStart, At: at(3)})
	s.SetIdentity(d, Identity{Alias: "tmp", Address: "pi:tmp"})
	if d.Special {
		t.Fatal("ephemeral special leaked to another session")
	}
	if v := d.View(); v.Title != "tmp" {
		t.Fatalf("title fallback: %q", v.Title)
	}
}

func TestStoreConcurrentUpdates(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "porter", "state.json")}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := st.Update(func(s *State) error {
				_, err := s.Apply(Event{Agent: string(rune('a' + i)), Kind: KindPrompt, At: at(i)})
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Agents) != 20 {
		t.Fatalf("lost updates: %d agents", len(s.Agents))
	}
}
