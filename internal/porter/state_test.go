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

func TestEndRemovesAndBadInputRejected(t *testing.T) {
	s := NewState()
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(0)})
	if a := mustApply(t, s, Event{Agent: "a", Kind: KindEnd, At: at(1)}); a != nil || len(s.Agents) != 0 {
		t.Fatal("end did not remove agent")
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
	l := s.List()
	if l[0].ID != "old-needs" || l[1].ID != "recent" || l[2].ID != "older" {
		t.Fatalf("order: %s %s %s", l[0].ID, l[1].ID, l[2].ID)
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
