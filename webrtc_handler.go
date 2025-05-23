package main

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type CustomSDPType string

const (
	CustomSDPTypeOffer  CustomSDPType = "offer"
	CustomSDPTypeAnswer CustomSDPType = "answer"
)

type CustomSessionDescription struct {
	Type CustomSDPType `json:"type"`
	SDP  string        `json:"sdp"`
}

type CustomICECandidateInit struct {
	Candidate        string  `json:"candidate"`
	SDPMid           string  `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment string  `json:"usernameFragment,omitempty"`
}

type DirectSignalPayloadClientToServer struct {
	SDP       *CustomSessionDescription `json:"sdp,omitempty"`
	Candidate *CustomICECandidateInit   `json:"candidate,omitempty"`
	ToPeerID  string                    `json:"toPeerID"`
	ClientID  string                    `json:"clientId,omitempty"`
}

type PayloadWithFrom struct {
	SDP        *CustomSessionDescription `json:"sdp,omitempty"`
	Candidate  *CustomICECandidateInit   `json:"candidate,omitempty"`
	FromPeerID string                    `json:"fromPeerID"`
	ToPeerID   string                    `json:"toPeerID,omitempty"`
	ClientID   string                    `json:"clientId,omitempty"`
}

type PeerConnectionContext struct {
	ws       *websocket.Conn
	id       string
	mu       sync.Mutex
	isClosed bool
}

func NewPeerConnectionContext(ws *websocket.Conn, clientID string) (*PeerConnectionContext, error) {
	p := &PeerConnectionContext{
		ws: ws,
		id: clientID,
	}
	return p, nil
}

func (p *PeerConnectionContext) GetID() string {
	return p.id
}

func (p *PeerConnectionContext) isContextClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isClosed
}

func (p *PeerConnectionContext) HandleMessages() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Peer %s: Recovered in HandleMessages: %v", p.id, r)
		}
		p.Close()
	}()

	for {
		if p.isContextClosed() {
			return
		}

		messageTypeNum, rawMsg, err := p.ws.ReadMessage()
		if err != nil {
			if !p.isContextClosed() {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
					log.Printf("Peer %s: WebSocket read error: %v. MessageType: %d", p.id, err, messageTypeNum)
				} else {
					log.Printf("Peer %s: WebSocket connection closed by client or network error. Code: %d, Error: %v", p.id, messageTypeNum, err)
				}
			}
			return
		}

		log.Printf("Peer %s: RAW MESSAGE RECEIVED: %s", p.id, string(rawMsg))

		var msg Message
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			log.Printf("Peer %s: Error unmarshalling message: %v. Raw: %s", p.id, err, string(rawMsg))
			continue
		}

		switch msg.Type {
		case "signal-initiate-p2p", "direct-offer", "direct-answer", "direct-candidate":
			p.routeP2PMessage(msg)

		default:
			log.Printf("Peer %s: Unknown message type: %s", p.id, msg.Type)
		}
	}
}

func (p *PeerConnectionContext) routeP2PMessage(msg Message) {
	var clientPayload DirectSignalPayloadClientToServer
	if err := json.Unmarshal(msg.Payload, &clientPayload); err != nil {
		log.Printf("Peer %s: Error unmarshalling P2P client payload for type %s: %v. Raw: %s", p.id, msg.Type, err, string(msg.Payload))
		return
	}

	relayPayload := PayloadWithFrom{
		SDP:        clientPayload.SDP,
		Candidate:  clientPayload.Candidate,
		FromPeerID: p.id,
		ToPeerID:   clientPayload.ToPeerID,
		ClientID:   clientPayload.ClientID,
	}

	marshaledRelayPayload, err := json.Marshal(relayPayload)
	if err != nil {
		log.Printf("Peer %s: Error marshalling P2P relay payload for type %s: %v", p.id, msg.Type, err)
		return
	}
	messageToRelay := Message{Type: msg.Type, Payload: marshaledRelayPayload}

	streamerLock.RLock()
	defer streamerLock.RUnlock()

	if msg.Type == "signal-initiate-p2p" {
		announcerID := p.id
		log.Printf("Peer %s is announcing 'signal-initiate-p2p'. Broadcasting to others and informing announcer about existing peers.", announcerID)

		foundOtherPeers := false
		var targets []*PeerConnectionContext
		var existingPeerIDs []string

		broadcastPayload := PayloadWithFrom{
			FromPeerID: announcerID,
			SDP:        clientPayload.SDP,
			Candidate:  clientPayload.Candidate,
			ClientID:   clientPayload.ClientID,
			ToPeerID:   clientPayload.ToPeerID,
		}
		marshaledBroadcastPayload, mErr := json.Marshal(broadcastPayload)
		if mErr != nil {
			log.Printf("Peer %s: Error marshalling 'signal-initiate-p2p' broadcast payload: %v", announcerID, mErr)
			return
		}
		messageToBroadcast := Message{Type: "signal-initiate-p2p", Payload: marshaledBroadcastPayload}

		for _, existingPeerCtx := range streamerConnections {
			if existingPeerCtx.GetID() == announcerID {
				continue
			}
			targets = append(targets, existingPeerCtx)
			existingPeerIDs = append(existingPeerIDs, existingPeerCtx.GetID())
			foundOtherPeers = true
		}

		for _, targetCtx := range targets {
			log.Printf("Peer %s (announcer) signaling 'signal-initiate-p2p' to existing peer %s", announcerID, targetCtx.GetID())
			targetCtx.sendMessage(messageToBroadcast)
		}

		for _, existingPeerID := range existingPeerIDs {
			payloadForAnnouncer := PayloadWithFrom{FromPeerID: existingPeerID}
			marshaledPayloadForAnnouncer, mErr := json.Marshal(payloadForAnnouncer)
			if mErr != nil {
				log.Printf("Error marshalling 'signal-initiate-p2p' payload for announcer %s about existing peer %s: %v", announcerID, existingPeerID, mErr)
				continue
			}
			msgForAnnouncer := Message{Type: "signal-initiate-p2p", Payload: marshaledPayloadForAnnouncer}
			log.Printf("Existing peer %s signaling 'signal-initiate-p2p' back to new peer %s (announcer)", existingPeerID, announcerID)
			p.sendMessage(msgForAnnouncer)
		}

		if !foundOtherPeers {
			log.Printf("Peer %s (announcer): No other peers found during 'signal-initiate-p2p'.", announcerID)
		}

	} else {
		targetPeerID := clientPayload.ToPeerID
		if targetPeerID == "" {
			log.Printf("Peer %s: P2P message type %s is missing 'toPeerID' in payload.", p.id, msg.Type)
			return
		}

		foundTarget := false
		var targetCtx *PeerConnectionContext
		for _, ctx := range streamerConnections {
			if ctx.GetID() == targetPeerID {
				targetCtx = ctx
				foundTarget = true
				break
			}
		}

		if foundTarget && targetCtx != nil {
			log.Printf("Peer %s routing P2P message '%s' (from %s) to target peer %s", p.id, msg.Type, relayPayload.FromPeerID, targetPeerID)
			targetCtx.sendMessage(messageToRelay)
		} else {
			log.Printf("Peer %s: Target peer %s for P2P message type '%s' not found.", p.id, targetPeerID, msg.Type)
		}
	}
}

func (p *PeerConnectionContext) sendMessage(msg Message) {
	p.mu.Lock()
	wsRef := p.ws
	isCtxClosed := p.isClosed
	p.mu.Unlock()

	if isCtxClosed || wsRef == nil {
		log.Printf("Peer %s: sendMessage called but context/ws is closed/nil. Type: %s. Skipping.", p.id, msg.Type)
		return
	}

	if err := wsRef.WriteJSON(msg); err != nil {
		log.Printf("Peer %s: Error writing JSON (type: %s) to WebSocket: %v", p.id, msg.Type, err)
	}
}

func (p *PeerConnectionContext) Close() {
	p.mu.Lock()
	if p.isClosed {
		p.mu.Unlock()
		return
	}
	p.isClosed = true
	log.Printf("Peer %s: Closing PeerConnectionContext...", p.id)

	wsRef := p.ws

	p.ws = nil
	p.mu.Unlock()

	if wsRef != nil {
		if err := wsRef.Close(); err != nil {
			log.Printf("Peer %s: Error closing WebSocket in PeerConnectionContext.Close: %v", p.id, err)
		}
	}

	log.Printf("Peer %s: PeerConnectionContext closed.", p.id)
}
