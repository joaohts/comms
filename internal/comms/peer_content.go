package comms

// PeerContent is the model-facing view of a received message. Delivery fencing,
// retries, recipient routing and acknowledgments stay inside the receiver.
type PeerContent struct {
	Kind                string `json:"kind"`
	Authority           string `json:"authority"`
	OriginAuthenticated bool   `json:"origin_authenticated"`
	ClaimsVerified      bool   `json:"claims_verified"`
	MessageID           string `json:"message_id"`
	SenderMachine       string `json:"sender_machine_id"`
	SenderAgent         string `json:"sender_agent_id"`
	Body                string `json:"body"`
	// Trust is porter's label when porter is on; see docs/PORTER.md.
	Trust string `json:"trust,omitempty"`
}

func ContentForPeer(m Message) PeerContent {
	return PeerContent{
		Kind: "comms_peer_message", Authority: "external_peer_content",
		OriginAuthenticated: true, ClaimsVerified: false,
		MessageID: m.ID, SenderMachine: m.SenderMachine, SenderAgent: m.SenderAgent, Body: m.Body, Trust: m.Trust,
	}
}
