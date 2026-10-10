package porter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/crypto/nacl/box"
)

// ExpoURL is Expo's push send endpoint.
const ExpoURL = "https://exp.host/--/api/v2/push/send"

// MaxPushData bounds the encoded push data. Expo rejects payloads over 4 KiB.
const MaxPushData = 3072

// Notice is the plaintext of an encrypted push. It never carries prompts,
// transcripts or tool input beyond the short needs text.
type Notice struct {
	Kind    string     `json:"kind"`
	Machine string     `json:"machine"`
	Agent   string     `json:"agent"`
	Title   string     `json:"title,omitempty"`
	Status  string     `json:"status"`
	Needs   *Needs     `json:"needs,omitempty"`
	Error   *ErrorInfo `json:"error,omitempty"`
	Summary string     `json:"summary,omitempty"`
}

func NoticeFor(machine string, a Agent) Notice {
	n := Notice{Kind: "agent", Machine: machine, Agent: a.ID, Title: clip(a.Title, 120), Status: a.Status, Summary: clip(a.Summary, 500)}
	if a.Needs != nil {
		n.Needs = &Needs{Kind: clip(a.Needs.Kind, 40), Text: clip(a.Needs.Text, 300)}
	}
	if a.Error != nil {
		n.Error = &ErrorInfo{Kind: clip(a.Error.Kind, 40)}
	}
	return n
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// PushData encrypts n with NaCl box from this node's private key to the
// subscriber's pinned public key: base64(nonce || box). The recipient can
// verify the sender, unlike a sealed box. from is this node's machine id so
// the app knows which pinned key opens it.
func PushData(n any, from string, recipientPub, senderPriv []byte) (map[string]string, error) {
	if len(recipientPub) != 32 || len(senderPriv) != 32 {
		return nil, fmt.Errorf("expected 32-byte keys")
	}
	var pub, priv [32]byte
	copy(pub[:], recipientPub)
	copy(priv[:], senderPriv)
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	sealed := box.Seal(nonce[:], plain, &nonce, &pub, &priv)
	data := map[string]string{"porter": "1", "from": from, "box": base64.StdEncoding.EncodeToString(sealed)}
	if b, _ := json.Marshal(data); len(b) > MaxPushData {
		return nil, fmt.Errorf("push payload too large (%d bytes)", len(b))
	}
	return data, nil
}

// NotifyPushData encrypts a notification push: the whole notify when it
// fits, otherwise a wake asking the app to fetch it from its comms inbox.
func NotifyPushData(n Notify, from string, recipientPub, senderPriv []byte) (map[string]string, error) {
	data, err := PushData(map[string]any{"kind": "notify", "notify": n}, from, recipientPub, senderPriv)
	if err == nil {
		return data, nil
	}
	return PushData(map[string]any{"kind": "wake", "id": n.ID}, from, recipientPub, senderPriv)
}

// ErrDeviceNotRegistered means the token is dead and should be forgotten.
var ErrDeviceNotRegistered = errors.New("push token not registered")

// Pusher delivers one data-only push to a device token.
type Pusher interface {
	Push(ctx context.Context, token string, data map[string]string) error
}

// Expo sends data-only, high-priority pushes through Expo's push service.
type Expo struct {
	URL  string
	HTTP *http.Client
}

func (e Expo) Push(ctx context.Context, token string, data map[string]string) error {
	url := e.URL
	if url == "" {
		url = ExpoURL
	}
	client := e.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	// No title or body: Android hands data-only messages to the app, which
	// decrypts and shows its own local notification.
	body, err := json.Marshal([]map[string]any{{"to": token, "data": data, "priority": "high"}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != 200 {
		return fmt.Errorf("expo push: HTTP %d", res.StatusCode)
	}
	var out struct {
		Data []struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details struct {
				Error string `json:"error"`
			} `json:"details"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) != 1 {
		return fmt.Errorf("expo push: unexpected response")
	}
	if t := out.Data[0]; t.Status != "ok" {
		if t.Details.Error == "DeviceNotRegistered" {
			return ErrDeviceNotRegistered
		}
		return fmt.Errorf("expo push: %s %s", t.Details.Error, t.Message)
	}
	return nil
}
