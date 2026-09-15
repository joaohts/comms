package comms

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

func validatePublicKey(public []byte) error {
	if len(public) != 32 {
		return problem(400, "bad_key", "public key must contain 32 bytes")
	}
	var probe [32]byte
	probe[0] = 1
	if _, err := curve25519.X25519(probe[:], public); err != nil {
		return problem(400, "bad_key", "invalid low-order X25519 public key")
	}
	return nil
}

func GenerateIdentity() (Identity, error) {
	pub, priv, e := box.GenerateKey(rand.Reader)
	if e != nil {
		return Identity{}, e
	}
	return Identity{NewID("m_"), pub[:], priv[:], Now()}, nil
}
func Encrypt(identity Identity, peer Peer, p Payload) (Envelope, error) {
	if p.Sender != identity.MachineID || p.Recipient != peer.MachineID {
		return Envelope{}, fmt.Errorf("identity mismatch")
	}
	pub, e := key32(peer.PublicKey)
	if e != nil {
		return Envelope{}, e
	}
	priv, e := key32(identity.PrivateKey)
	if e != nil {
		return Envelope{}, e
	}
	var nonce [24]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return Envelope{}, e
	}
	return Envelope{p.Routing, nonce[:], box.Seal(nil, marshal(p), &nonce, pub, priv)}, nil
}
func Decrypt(identity Identity, peer Peer, env Envelope) (Payload, error) {
	var p Payload
	if e := validateRouting(env.Routing); e != nil {
		return p, e
	}
	if env.Sender != peer.MachineID || env.Recipient != identity.MachineID {
		return p, problem(403, "wrong_identity", "envelope does not match pinned identities")
	}
	return openPayload(identity, peer, env, true)
}
func openPayload(identity Identity, peer Peer, env Envelope, incoming bool) (Payload, error) {
	var p Payload
	pub, e := key32(peer.PublicKey)
	if e != nil {
		return p, e
	}
	priv, e := key32(identity.PrivateKey)
	if e != nil {
		return p, e
	}
	if len(env.Nonce) != 24 || len(env.Ciphertext) > MaxWire {
		return p, problem(400, "bad_ciphertext", "invalid ciphertext size")
	}
	var nonce [24]byte
	copy(nonce[:], env.Nonce)
	plain, ok := box.Open(nil, env.Ciphertext, &nonce, pub, priv)
	if !ok {
		return p, problem(403, "authentication_failed", "ciphertext authentication failed")
	}
	d := json.NewDecoder(bytes.NewReader(plain))
	d.DisallowUnknownFields()
	if e = d.Decode(&p); e != nil {
		return p, problem(400, "bad_payload", "invalid encrypted payload")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return p, problem(400, "bad_payload", "one encrypted JSON payload required")
	}
	if p.Routing != env.Routing {
		return p, problem(403, "routing_tampered", "authenticated routing differs from envelope")
	}
	if p.Kind == "message" && (len([]byte(p.Body)) > MaxBody || !validWireID(p.SenderAgent) || !validWireID(p.RecipientAgent) || p.Receipt != nil) {
		return p, problem(400, "bad_payload", "invalid message payload")
	}
	if p.Kind == "receipt" && (p.Receipt == nil || p.Body != "" || len(plain) > 4096) {
		return p, problem(400, "bad_receipt", "invalid receipt payload")
	}
	if p.Kind == "receipt" && !validReceiptFailure(p.Receipt.Failure) {
		return p, problem(400, "bad_receipt", "receipt failure reason must be a fixed protocol code")
	}
	return p, nil
}

func validReceiptFailure(code string) bool {
	switch code {
	case "", "agent_ended", "grant_revoked", "local_only", "expired", "receiver_output_failed", "receiver_disconnected", "receiver_disconnected_during_handoff", "node_restarted_during_handoff", "node_stopped_during_handoff", "node_stopping", "handoff_timeout", "adapter_error", "operator_cancelled", "operator_confirmed":
		return true
	}
	return false
}

// Queries are synchronous API data and never durable message-envelope kinds.
// Both directions bind the purpose, request ID, pinned machine IDs and deadline.
func SealQuery(identity Identity, peer Peer, p QueryPayload) ([]byte, error) {
	pub, e := key32(peer.PublicKey)
	if e != nil {
		return nil, e
	}
	priv, e := key32(identity.PrivateKey)
	if e != nil {
		return nil, e
	}
	var nonce [24]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return nil, e
	}
	return box.Seal(nonce[:], marshal(p), &nonce, pub, priv), nil
}
func OpenQuery(identity Identity, peer Peer, sealed []byte) (QueryPayload, error) {
	var p QueryPayload
	if len(sealed) < 40 || len(sealed) > 2*MaxHistory {
		return p, problem(400, "bad_query", "invalid query size")
	}
	pub, e := key32(peer.PublicKey)
	if e != nil {
		return p, e
	}
	priv, e := key32(identity.PrivateKey)
	if e != nil {
		return p, e
	}
	var n [24]byte
	copy(n[:], sealed[:24])
	plain, ok := box.Open(nil, sealed[24:], &n, pub, priv)
	if !ok {
		return p, problem(403, "authentication_failed", "query authentication failed")
	}
	if e = json.Unmarshal(plain, &p); e != nil {
		return p, e
	}
	if p.Sender != peer.MachineID || p.Recipient != identity.MachineID || p.ID == "" || p.Deadline < Now() {
		return p, problem(403, "bad_query", "query identity/deadline mismatch")
	}
	if p.Purpose != "comms-history-query-v1" && p.Purpose != "comms-history-result-v1" {
		return p, problem(400, "bad_query", "wrong query purpose")
	}
	return p, nil
}
func SameKey(a, b []byte) bool { return len(a) == 32 && bytes.Equal(a, b) }
