package comms

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const ServiceKeyHeader = "X-Comms-Service-Key"

// An explicit file is fail-closed: missing, empty or public files never disable
// protection. The same option supplies the outbound key on a consumer node.
func loadServiceKey(cfg *Config) error {
	if cfg.BrokerServiceKeyFile != "" {
		info, err := os.Stat(cfg.BrokerServiceKeyFile)
		if err != nil {
			return fmt.Errorf("broker service key file unavailable: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
			return fmt.Errorf("broker service key file must be a private regular file (mode 0600, at most 4096 bytes)")
		}
		data, err := os.ReadFile(cfg.BrokerServiceKeyFile)
		if err != nil {
			return fmt.Errorf("broker service key file unreadable: %w", err)
		}
		cfg.BrokerServiceKey = strings.TrimSpace(string(data))
		if cfg.BrokerServiceKey == "" {
			return fmt.Errorf("broker service key file is empty")
		}
	}
	if cfg.BrokerServiceKey != "" {
		if len(cfg.BrokerServiceKey) < 32 || len(cfg.BrokerServiceKey) > 4096 {
			return fmt.Errorf("broker service key must contain 32–4096 printable non-space ASCII characters")
		}
		for _, c := range cfg.BrokerServiceKey {
			if c < 33 || c > 126 {
				return fmt.Errorf("invalid broker service key characters")
			}
		}
	}
	return nil
}

func (b *Broker) serviceKeyGate(next http.Handler) http.Handler {
	if b.cfg.BrokerServiceKey == "" {
		return next
	}
	expected := sha256.Sum256([]byte(b.cfg.BrokerServiceKey))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(ServiceKeyHeader)
		provided := sha256.Sum256([]byte(r.Header.Get(ServiceKeyHeader)))
		if len(values) != 1 || subtle.ConstantTimeCompare(expected[:], provided[:]) != 1 {
			w.Header().Set("Cache-Control", "no-store")
			brokerError(w, problem(401, "service_key_required", "valid broker service API key required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func noBrokerRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
