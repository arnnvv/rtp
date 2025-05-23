package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
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

type ServerSignalPayload struct {
	SDP       *CustomSessionDescription `json:"sdp,omitempty"`
	Candidate *CustomICECandidateInit   `json:"candidate,omitempty"`
}

type PeerConnectionContext struct {
	ws             *websocket.Conn
	id             string
	mu             sync.Mutex
	isClosed       bool
	peerConnection *webrtc.PeerConnection
	videoWriter    *ivfwriter.IVFWriter
	audioWriter    *oggwriter.OggWriter
	hlsProcess     *exec.Cmd
	videoFile      *os.File
	audioFile      *os.File
	isStreaming    bool
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

func (p *PeerConnectionContext) createServerPeerConnection() error {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return err
	}

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			candidateJSON := candidate.ToJSON()

			var sdpMid string
			if candidateJSON.SDPMid != nil {
				sdpMid = *candidateJSON.SDPMid
			}

			var usernameFragment string
			if candidateJSON.UsernameFragment != nil {
				usernameFragment = *candidateJSON.UsernameFragment
			}

			p.sendMessage(Message{
				Type: "server-candidate",
				Payload: mustMarshal(ServerSignalPayload{
					Candidate: &CustomICECandidateInit{
						Candidate:        candidateJSON.Candidate,
						SDPMid:           sdpMid,
						SDPMLineIndex:    candidateJSON.SDPMLineIndex,
						UsernameFragment: usernameFragment,
					},
				}),
			})
		}
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("Peer %s: Received track of type: %s", p.id, track.Kind().String())
		p.handleIncomingTrack(track)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Peer %s: Server connection state changed: %s", p.id, state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			p.isStreaming = true
		case webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			p.stopHLSTranscoding()
		}
	})

	p.peerConnection = pc
	return nil
}

func (p *PeerConnectionContext) handleIncomingTrack(track *webrtc.TrackRemote) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Peer %s: Recovered in handleIncomingTrack: %v", p.id, r)
		}
	}()

	outputDir := fmt.Sprintf("./hls-output/%s", p.id)
	os.MkdirAll(outputDir, 0755)

	if track.Kind() == webrtc.RTPCodecTypeVideo {
		videoFilePath := fmt.Sprintf("%s/video.ivf", outputDir)
		videoFile, err := os.Create(videoFilePath)
		if err != nil {
			log.Printf("Peer %s: Error creating video file: %v", p.id, err)
			return
		}
		p.videoFile = videoFile

		videoWriter, err := ivfwriter.New(videoFilePath)
		if err != nil {
			log.Printf("Peer %s: Error creating video writer: %v", p.id, err)
			return
		}
		p.videoWriter = videoWriter

		go p.saveVideoTrack(track)
	} else if track.Kind() == webrtc.RTPCodecTypeAudio {
		audioFilePath := fmt.Sprintf("%s/audio.ogg", outputDir)
		audioFile, err := os.Create(audioFilePath)
		if err != nil {
			log.Printf("Peer %s: Error creating audio file: %v", p.id, err)
			return
		}
		p.audioFile = audioFile

		audioWriter, err := oggwriter.New(audioFilePath, 48000, 2)
		if err != nil {
			log.Printf("Peer %s: Error creating audio writer: %v", p.id, err)
			return
		}
		p.audioWriter = audioWriter

		go p.saveAudioTrack(track)
	}

	if p.videoWriter != nil && p.audioWriter != nil && p.hlsProcess == nil {
		go func() {
			time.Sleep(2 * time.Second)
			p.startHLSTranscoding()
		}()
	}
}

func (p *PeerConnectionContext) saveVideoTrack(track *webrtc.TrackRemote) {
	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			log.Printf("Peer %s: Error reading video RTP: %v", p.id, err)
			return
		}

		if p.videoWriter != nil {
			err = p.videoWriter.WriteRTP(rtpPacket)
			if err != nil {
				log.Printf("Peer %s: Error writing video RTP: %v", p.id, err)
				return
			}
		}
	}
}

func (p *PeerConnectionContext) saveAudioTrack(track *webrtc.TrackRemote) {
	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			log.Printf("Peer %s: Error reading audio RTP: %v", p.id, err)
			return
		}

		if p.audioWriter != nil {
			err = p.audioWriter.WriteRTP(rtpPacket)
			if err != nil {
				log.Printf("Peer %s: Error writing audio RTP: %v", p.id, err)
				return
			}
		}
	}
}

func (p *PeerConnectionContext) startHLSTranscoding() {
	outputDir := fmt.Sprintf("./hls-output/%s", p.id)
	videoPath := fmt.Sprintf("%s/video.ivf", outputDir)
	audioPath := fmt.Sprintf("%s/audio.ogg", outputDir)
	playlistPath := fmt.Sprintf("%s/playlist.m3u8", outputDir)

	cmd := exec.Command("ffmpeg",
		"-re",
		"-i", videoPath,
		"-i", audioPath,
		"-c:v", "libx264",
		"-c:a", "aac",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-g", "30",
		"-sc_threshold", "0",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "5",
		"-hls_flags", "delete_segments+append_list",
		"-hls_allow_cache", "0",
		playlistPath,
	)

	log.Printf("Peer %s: Starting HLS transcoding", p.id)

	p.hlsProcess = cmd
	err := cmd.Start()
	if err != nil {
		log.Printf("Peer %s: Error starting HLS transcoding: %v", p.id, err)
		return
	}

	go func() {
		err := cmd.Wait()
		if err != nil {
			log.Printf("Peer %s: HLS transcoding process ended with error: %v", p.id, err)
		} else {
			log.Printf("Peer %s: HLS transcoding process ended successfully", p.id)
		}
	}()
}

func (p *PeerConnectionContext) stopHLSTranscoding() {
	if p.hlsProcess != nil {
		log.Printf("Peer %s: Stopping HLS transcoding", p.id)
		p.hlsProcess.Process.Kill()
		p.hlsProcess = nil
	}

	if p.videoWriter != nil {
		p.videoWriter.Close()
		p.videoWriter = nil
	}

	if p.audioWriter != nil {
		p.audioWriter.Close()
		p.audioWriter = nil
	}

	if p.videoFile != nil {
		p.videoFile.Close()
		p.videoFile = nil
	}

	if p.audioFile != nil {
		p.audioFile.Close()
		p.audioFile = nil
	}
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
		case "server-offer":
			p.handleServerOffer(msg)
		case "server-candidate":
			p.handleServerCandidate(msg)
		default:
			log.Printf("Peer %s: Unknown message type: %s", p.id, msg.Type)
		}
	}
}

func (p *PeerConnectionContext) handleServerOffer(msg Message) {
	var payload ServerSignalPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Peer %s: Error unmarshalling server offer payload: %v", p.id, err)
		return
	}

	if payload.SDP == nil {
		log.Printf("Peer %s: Server offer missing SDP", p.id)
		return
	}

	if p.peerConnection == nil {
		if err := p.createServerPeerConnection(); err != nil {
			log.Printf("Peer %s: Error creating server peer connection: %v", p.id, err)
			return
		}
	}

	sdp := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  payload.SDP.SDP,
	}

	if err := p.peerConnection.SetRemoteDescription(sdp); err != nil {
		log.Printf("Peer %s: Error setting remote description: %v", p.id, err)
		return
	}

	answer, err := p.peerConnection.CreateAnswer(nil)
	if err != nil {
		log.Printf("Peer %s: Error creating answer: %v", p.id, err)
		return
	}

	if err := p.peerConnection.SetLocalDescription(answer); err != nil {
		log.Printf("Peer %s: Error setting local description: %v", p.id, err)
		return
	}

	p.sendMessage(Message{
		Type: "server-answer",
		Payload: mustMarshal(ServerSignalPayload{
			SDP: &CustomSessionDescription{
				Type: CustomSDPTypeAnswer,
				SDP:  answer.SDP,
			},
		}),
	})
}

func (p *PeerConnectionContext) handleServerCandidate(msg Message) {
	var payload ServerSignalPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Peer %s: Error unmarshalling server candidate payload: %v", p.id, err)
		return
	}

	if payload.Candidate == nil || p.peerConnection == nil {
		return
	}

	candidate := webrtc.ICECandidateInit{
		Candidate:        payload.Candidate.Candidate,
		SDPMid:           &payload.Candidate.SDPMid,
		SDPMLineIndex:    payload.Candidate.SDPMLineIndex,
		UsernameFragment: &payload.Candidate.UsernameFragment,
	}

	if err := p.peerConnection.AddICECandidate(candidate); err != nil {
		log.Printf("Peer %s: Error adding ICE candidate: %v", p.id, err)
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

	p.stopHLSTranscoding()

	if p.peerConnection != nil {
		p.peerConnection.Close()
	}

	if wsRef != nil {
		if err := wsRef.Close(); err != nil {
			log.Printf("Peer %s: Error closing WebSocket in PeerConnectionContext.Close: %v", p.id, err)
		}
	}

	log.Printf("Peer %s: PeerConnectionContext closed.", p.id)
}

func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
