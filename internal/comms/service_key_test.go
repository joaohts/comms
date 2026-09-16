package comms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBrokerServiceKeyGatesRegistrationAndEveryRoute(t *testing.T) {
	key := strings.Repeat("s", 64)
	f := testBroker(t, func(c *Config) { c.BrokerServiceKey = key })
	for _, path := range []string{"/v1/auth/challenges", "/v1/auth/complete", "/v1/who", "/v1/stream", "/v1/messages", "/v1/receipts", "/v1/acks", "/v1/history/peer", "/v1/history-results/id", "/unknown"} {
		for _, supplied := range []string{"", "wrong-key"} {
			r := httptest.NewRequest("POST", path, strings.NewReader("not JSON"))
			if supplied != "" {
				r.Header.Set(ServiceKeyHeader, supplied)
			}
			w := httptest.NewRecorder()
			f.b.Handler().ServeHTTP(w, r)
			var result APIError
			json.Unmarshal(w.Body.Bytes(), &result)
			if w.Code != 401 || result.Code != "service_key_required" {
				t.Fatalf("gate bypass: %s: %d %s", path, w.Code, w.Body)
			}
		}
	}
	r := httptest.NewRequest("GET", "/v1/who", nil)
	r.Header.Set(ServiceKeyHeader, key)
	w := httptest.NewRecorder()
	f.b.Handler().ServeHTTP(w, r)
	if w.Code != 401 || !strings.Contains(w.Body.String(), `"code":"unauthorized"`) {
		t.Fatalf("service key bypassed machine authentication: %s", w.Body)
	}
	r.Header.Add(ServiceKeyHeader, key)
	w = httptest.NewRecorder()
	f.b.Handler().ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "service_key_required") {
		t.Fatal("duplicate key headers accepted")
	}
	valid := newNodeIntegration(t, func(c *Config) { c.BrokerServiceKey = key })
	if err := valid.node.ConfigureBroker(context.Background(), f.server.URL); err != nil {
		t.Fatal(err)
	}
	nodeIntegrationEventually(t, "keyed broker stream connects", func() bool { return valid.node.brokerOnline.Load() })
	invalid := newNodeIntegration(t, nil)
	if err := invalid.node.ConfigureBroker(context.Background(), f.server.URL); errorCode(err) != "service_key_required" {
		t.Fatalf("unkeyed registration accepted: %v", err)
	}
}

func TestServiceKeyFileFailsClosed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	cfg := Config{BrokerServiceKeyFile: p}
	if loadServiceKey(&cfg) == nil {
		t.Fatal("missing explicit file accepted")
	}
	for _, content := range []string{"", "short", strings.Repeat("x", 32) + "\nInjected: header"} {
		os.WriteFile(p, []byte(content), 0600)
		if loadServiceKey(&cfg) == nil {
			t.Fatal("invalid key accepted")
		}
	}
	os.WriteFile(p, []byte(strings.Repeat("a", 64)+"\n"), 0600)
	if err := loadServiceKey(&cfg); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, 0644)
	if loadServiceKey(&cfg) == nil {
		t.Fatal("public file accepted")
	}
	encoded, _ := json.Marshal(cfg)
	if strings.Contains(string(encoded), strings.Repeat("a", 64)) {
		t.Fatal("key serialized")
	}
}

func TestBrokerServiceKeyNeverFollowsRedirects(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	n := newNodeIntegration(t, func(c *Config) { c.BrokerServiceKey = strings.Repeat("k", 64) })
	if err := n.node.rawRequest(context.Background(), origin.URL, "token", "GET", "/test", nil, nil); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() {
		t.Fatal("redirect target received request")
	}
}
