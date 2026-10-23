package comms

import (
	"fmt"
	"testing"
	"time"
)

// A backlog for one recipient must not fill the outbound window and starve
// another recipient queued behind it (the porter→phone jam of 2026-10).
func TestPendingOutgoingOneHeadPerRecipient(t *testing.T) {
	s := cryptoStoreFixture(t, nil)
	a, e := s.OpenAgent(OpenRequest{Alias: "porter", Harness: "service", HarnessID: "porter"})
	if e != nil {
		t.Fatal(e)
	}
	exp := time.Now().Add(time.Hour).UnixMilli()
	base := Now() - 10_000
	insert := func(id, machine, agent string, at int64) {
		t.Helper()
		m := Message{ID: id, SenderMachine: s.Identity.MachineID, SenderAgent: a.Agent.ID, RecipientMachine: machine, RecipientAgent: agent,
			Kind: "message", Wire: []byte(`{}`), State: "queued", CreatedAt: at, ExpiresAt: exp, Hash: id}
		if _, _, e := s.Insert(m, false); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 300; i++ {
		insert(fmt.Sprintf("msg_phone_%03d", i), "m_phone", "a_app", base+int64(i))
	}
	insert("msg_pi_000", "m_pi", "a_brain", base+1000)

	got, e := s.PendingOutgoing(256)
	if e != nil {
		t.Fatal(e)
	}
	byRecipient := map[string][]string{}
	for _, m := range got {
		byRecipient[m.RecipientMachine] = append(byRecipient[m.RecipientMachine], m.ID)
	}
	if ids := byRecipient["m_phone"]; len(ids) != 1 || ids[0] != "msg_phone_000" {
		t.Fatalf("phone backlog should expose only its head, got %v", ids)
	}
	if ids := byRecipient["m_pi"]; len(ids) != 1 {
		t.Fatalf("message to another recipient was starved: %v", got)
	}
}

func TestSendTTL(t *testing.T) {
	cases := []struct {
		ms   int64
		want time.Duration
	}{
		{0, MessageTTL},
		{-5, MessageTTL},
		{PorterTopicTTL.Milliseconds(), PorterTopicTTL},
		{(MessageTTL + time.Hour).Milliseconds(), MessageTTL},
	}
	for _, c := range cases {
		if got := sendTTL(c.ms); got != c.want {
			t.Errorf("sendTTL(%d) = %v, want %v", c.ms, got, c.want)
		}
	}
}
