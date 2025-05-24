package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/h264writer"
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
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
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

type VideoWriter interface {
	WriteRTP(packet *rtp.Packet) error
	Close() error
}

type CallSessionManager struct {
	participants map[string]*PeerConnectionContext
	videoWriters map[string]VideoWriter
	audioWriters map[string]*oggwriter.OggWriter
	videoFiles   map[string]*os.File
	audioFiles   map[string]*os.File
	hlsProcess   *exec.Cmd
	isStreaming  bool
	mutex        sync.RWMutex
}

var globalCallSession = &CallSessionManager{
	participants: make(map[string]*PeerConnectionContext),
	videoWriters: make(map[string]VideoWriter),
	audioWriters: make(map[string]*oggwriter.OggWriter),
	videoFiles:   make(map[string]*os.File),
	audioFiles:   make(map[string]*os.File),
}

type PeerConnectionContext struct {
	ws             *websocket.Conn
	id             string
	mu             sync.Mutex
	isClosed       bool
	peerConnection *webrtc.PeerConnection
}

func NewPeerConnectionContext(ws *websocket.Conn, clientID string) (*PeerConnectionContext, error) {
	p := &PeerConnectionContext{
		ws: ws,
		id: clientID,
	}
	globalCallSession.mutex.Lock()
	globalCallSession.participants[clientID] = p
	globalCallSession.mutex.Unlock()
	log.Printf("Participant %s added to call. Total participants: %d", clientID, len(globalCallSession.participants))
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
	p.mu.Lock()
	if p.peerConnection != nil && p.peerConnection.SignalingState() != webrtc.SignalingStateClosed {
		p.mu.Unlock()
		log.Printf("Peer %s: Server peer connection already exists.", p.id)
		return nil
	}
	p.mu.Unlock()

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("failed to create peer connection: %w", err)
	}

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			candidateJSON := candidate.ToJSON()
			customCandidate := &CustomICECandidateInit{
				Candidate:        candidateJSON.Candidate,
				SDPMid:           candidateJSON.SDPMid,
				SDPMLineIndex:    candidateJSON.SDPMLineIndex,
				UsernameFragment: candidateJSON.UsernameFragment,
			}
			p.sendMessage(Message{
				Type: "server-candidate",
				Payload: mustMarshal(ServerSignalPayload{
					Candidate: customCandidate,
				}),
			})
		}
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("Peer %s: Received track. Type: %s, Codec: %s, SSRC: %d", p.id, track.Kind().String(), track.Codec().MimeType, track.SSRC())
		p.handleIncomingTrack(track)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Peer %s: Server peer connection state changed: %s", p.id, state.String())
	})

	p.mu.Lock()
	p.peerConnection = pc
	p.mu.Unlock()
	log.Printf("Peer %s: Server peer connection created.", p.id)
	return nil
}

func (p *PeerConnectionContext) handleIncomingTrack(track *webrtc.TrackRemote) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Peer %s: Recovered in handleIncomingTrack: %v", p.id, r)
		}
	}()

	outputDir := "./hls-output"
	os.MkdirAll(outputDir, 0755)

	if track.Kind() == webrtc.RTPCodecTypeVideo {
		codecMimeType := strings.ToLower(track.Codec().MimeType)
		log.Printf("Peer %s: Processing video track with MimeType: %s", p.id, codecMimeType)

		var videoWriterInstance VideoWriter
		var videoFilePath string
		var videoFile *os.File
		var err error

		switch codecMimeType {
		case "video/vp8", "video/vp9":
			videoFilePath = fmt.Sprintf("%s/video_%s.ivf", outputDir, p.id)
			videoFile, err = os.Create(videoFilePath)
			if err != nil {
				log.Printf("Peer %s: Error creating IVF video file %s: %v", p.id, videoFilePath, err)
				return
			}
			videoWriterInstance, err = ivfwriter.NewWith(videoFile, ivfwriter.WithCodec(track.Codec().MimeType))
			if err != nil {
				log.Printf("Peer %s: Error creating IVF video writer for %s (Mime: %s): %v", p.id, videoFilePath, codecMimeType, err)
				videoFile.Close()
				return
			}
			log.Printf("Peer %s: Created IVF video writer for %s", p.id, videoFilePath)
		case "video/h264":
			videoFilePath = fmt.Sprintf("%s/video_%s.h264", outputDir, p.id)
			videoFile, err = os.Create(videoFilePath)
			if err != nil {
				log.Printf("Peer %s: Error creating H264 video file %s: %v", p.id, videoFilePath, err)
				return
			}
			videoWriterInstance = h264writer.NewWith(videoFile)
			log.Printf("Peer %s: Created H264 video writer for %s", p.id, videoFilePath)
		default:
			log.Printf("Peer %s: Unsupported video codec for saving: %s. Skipping video track.", p.id, codecMimeType)
			return
		}

		log.Printf("Peer %s: Video file %s ready for writing.", p.id, videoFilePath)
		globalCallSession.mutex.Lock()
		globalCallSession.videoFiles[p.id] = videoFile
		globalCallSession.videoWriters[p.id] = videoWriterInstance
		globalCallSession.mutex.Unlock()

		go p.saveVideoTrack(track, videoWriterInstance)

	} else if track.Kind() == webrtc.RTPCodecTypeAudio {
		audioFilePath := fmt.Sprintf("%s/audio_%s.ogg", outputDir, p.id)
		audioFile, err := os.Create(audioFilePath)
		if err != nil {
			log.Printf("Peer %s: Error creating audio file %s: %v", p.id, audioFilePath, err)
			return
		}
		log.Printf("Peer %s: Created audio file: %s", p.id, audioFilePath)

		globalCallSession.mutex.Lock()
		globalCallSession.audioFiles[p.id] = audioFile
		globalCallSession.mutex.Unlock()

		audioWriter, err := oggwriter.NewWith(audioFile, track.Codec().ClockRate, track.Codec().Channels)
		if err != nil {
			log.Printf("Peer %s: Error creating audio writer for %s: %v", p.id, audioFilePath, err)
			audioFile.Close()
			return
		}
		log.Printf("Peer %s: Created audio writer for %s (ClockRate: %d, Channels: %d)", p.id, audioFilePath, track.Codec().ClockRate, track.Codec().Channels)

		globalCallSession.mutex.Lock()
		globalCallSession.audioWriters[p.id] = audioWriter
		globalCallSession.mutex.Unlock()

		go p.saveAudioTrack(track, audioWriter)
	}

	globalCallSession.checkAndStartCompositeStream()
}

func (p *PeerConnectionContext) saveVideoTrack(track *webrtc.TrackRemote, writer VideoWriter) {
	log.Printf("Peer %s: Starting to save video track (Codec: %s, SSRC: %d)", p.id, track.Codec().MimeType, track.SSRC())
	for {
		if p.isContextClosed() {
			log.Printf("Peer %s: Context closed, stopping video track saving.", p.id)
			return
		}
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			log.Printf("Peer %s: Error reading video RTP: %v. Stopping video track saving.", p.id, err)
			return
		}
		if writer != nil {
			if err := writer.WriteRTP(rtpPacket); err != nil {
				log.Printf("Peer %s: Error writing video RTP: %v. Stopping video track saving.", p.id, err)
				return
			}
		} else {
			log.Printf("Peer %s: Video writer is nil (possibly removed), cannot write RTP packet. Stopping video track saving.", p.id)
			return
		}
	}
}

func (p *PeerConnectionContext) saveAudioTrack(track *webrtc.TrackRemote, writer *oggwriter.OggWriter) {
	log.Printf("Peer %s: Starting to save audio track (Codec: %s, SSRC: %d)", p.id, track.Codec().MimeType, track.SSRC())
	for {
		if p.isContextClosed() {
			log.Printf("Peer %s: Context closed, stopping audio track saving.", p.id)
			return
		}
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			log.Printf("Peer %s: Error reading audio RTP: %v. Stopping audio track saving.", p.id, err)
			return
		}
		if writer != nil {
			if err := writer.WriteRTP(rtpPacket); err != nil {
				log.Printf("Peer %s: Error writing audio RTP: %v. Stopping audio track saving.", p.id, err)
				return
			}
		} else {
			log.Printf("Peer %s: Audio writer is nil (possibly removed), cannot write RTP packet. Stopping audio track saving.", p.id)
			return
		}
	}
}

func (csm *CallSessionManager) checkAndStartCompositeStream() {
	csm.mutex.Lock()

	log.Printf("checkAndStartCompositeStream: Participants: %d, VideoWriters: %d, AudioWriters: %d, isStreaming: %t",
		len(csm.participants), len(csm.videoWriters), len(csm.audioWriters), csm.isStreaming)

	if len(csm.participants) == 2 && len(csm.videoWriters) == 2 && len(csm.audioWriters) == 2 && !csm.isStreaming {
		validParticipantsWithMedia := 0
		var pIDs []string
		for id := range csm.participants {
			pIDs = append(pIDs, id)
			_, hasVideo := csm.videoWriters[id]
			_, hasAudio := csm.audioWriters[id]
			if hasVideo && hasAudio {
				validParticipantsWithMedia++
			}
		}

		if validParticipantsWithMedia == 2 {
			log.Printf("Two participants (%s, %s) with video/audio detected, starting composite stream in 5 seconds...", pIDs[0], pIDs[1])
			csm.mutex.Unlock()
			go func() {
				time.Sleep(5 * time.Second)
				csm.startCompositeHLS()
			}()
			return
		} else {
			log.Printf("Participant count is 2, but writer counts (%d video, %d audio) or valid media participants (%d) not ready.",
				len(csm.videoWriters), len(csm.audioWriters), validParticipantsWithMedia)
		}
	}
	csm.mutex.Unlock()
}

func (csm *CallSessionManager) validateInputFiles(paths ...string) bool {
	for _, path := range paths {
		stat, err := os.Stat(path)
		if err != nil {
			log.Printf("File validation: File %s does not exist: %v", path, err)
			return false
		}
		if stat.Size() < 2048 {
			log.Printf("File validation: File %s is too small (%d bytes), expected at least 2048 bytes.", path, stat.Size())
		}
		log.Printf("File validation: File %s exists with size %d bytes", path, stat.Size())
	}
	return true
}

func (csm *CallSessionManager) startCompositeHLS() {
	csm.mutex.Lock()

	if csm.isStreaming {
		log.Println("startCompositeHLS called but already streaming.")
		csm.mutex.Unlock()
		return
	}

	if len(csm.videoWriters) < 2 || len(csm.audioWriters) < 2 {
		log.Printf("startCompositeHLS called but not enough video (%d) or audio (%d) writers.", len(csm.videoWriters), len(csm.audioWriters))
		csm.mutex.Unlock()
		return
	}

	outputDir := "./hls-output"
	playlistPath := fmt.Sprintf("%s/playlist.m3u8", outputDir)

	var participantIDs []string
	for clientID := range csm.participants {
		if _, vOK := csm.videoWriters[clientID]; vOK {
			if _, aOK := csm.audioWriters[clientID]; aOK {
				participantIDs = append(participantIDs, clientID)
				if len(participantIDs) == 2 {
					break
				}
			}
		}
	}

	if len(participantIDs) < 2 {
		log.Printf("Could not identify two participants with both video and audio writers from active participants. Found %d.", len(participantIDs))
		csm.mutex.Unlock()
		return
	}
	participant1ID := participantIDs[0]
	participant2ID := participantIDs[1]

	video1File, ok1 := csm.videoFiles[participant1ID]
	if !ok1 {
		log.Printf("Video file for participant %s not found in videoFiles map", participant1ID)
		csm.mutex.Unlock()
		return
	}
	video1Path := video1File.Name()

	video2File, ok2 := csm.videoFiles[participant2ID]
	if !ok2 {
		log.Printf("Video file for participant %s not found in videoFiles map", participant2ID)
		csm.mutex.Unlock()
		return
	}
	video2Path := video2File.Name()

	audio1Path := fmt.Sprintf("%s/audio_%s.ogg", outputDir, participant1ID)
	audio2Path := fmt.Sprintf("%s/audio_%s.ogg", outputDir, participant2ID)

	if !csm.validateInputFiles(video1Path, audio1Path, video2Path, audio2Path) {
		log.Printf("Input file validation failed for HLS.")
		csm.mutex.Unlock()
		return
	}

	var video1InputFormat, video2InputFormat string
	if strings.HasSuffix(video1Path, ".ivf") {
		video1InputFormat = "ivf"
	} else if strings.HasSuffix(video1Path, ".h264") {
		video1InputFormat = "h264"
	} else {
		log.Printf("Unknown video file extension for %s", video1Path)
		csm.mutex.Unlock()
		return
	}

	if strings.HasSuffix(video2Path, ".ivf") {
		video2InputFormat = "ivf"
	} else if strings.HasSuffix(video2Path, ".h264") {
		video2InputFormat = "h264"
	} else {
		log.Printf("Unknown video file extension for %s", video2Path)
		csm.mutex.Unlock()
		return
	}

	os.Remove(playlistPath)
	segmentPattern := fmt.Sprintf("%s/segment_*.ts", outputDir)
	matches, _ := filepath.Glob(segmentPattern)
	for _, match := range matches {
		os.Remove(match)
	}

	cmdArgs := []string{
		"-y", "-re",
		"-fflags", "+genpts", "-avoid_negative_ts", "make_zero",

		"-f", video1InputFormat, "-i", video1Path,
		"-f", "ogg", "-i", audio1Path,

		"-f", video2InputFormat, "-i", video2Path,
		"-f", "ogg", "-i", audio2Path,

		"-filter_complex",
		"[0:v]scale=640:360,setpts=PTS-STARTPTS,fps=25[left];" +
			"[2:v]scale=640:360,setpts=PTS-STARTPTS,fps=25[right];" +
			"[left][right]hstack=inputs=2:shortest=1[v];" +
			"[1:a]asetpts=PTS-STARTPTS,volume=0.8[a1];" +
			"[3:a]asetpts=PTS-STARTPTS,volume=0.8[a2];" +
			"[a1][a2]amix=inputs=2:duration=shortest:normalize=0[a]",
		"-map", "[v]", "-map", "[a]",
		"-c:v", "libx264", "-preset", "superfast", "-tune", "zerolatency",
		"-profile:v", "baseline", "-level", "3.1",
		"-crf", "28", "-maxrate", "1000k", "-bufsize", "2000k",
		"-g", "25", "-keyint_min", "25", "-sc_threshold", "0", "-forced-idr", "1",
		"-c:a", "aac", "-b:a", "96k", "-ar", "44100", "-ac", "2",
		"-f", "hls", "-hls_time", "1", "-hls_list_size", "6",
		"-hls_flags", "delete_segments+append_list+omit_endlist",
		"-hls_allow_cache", "0", "-hls_segment_type", "mpegts", "-start_number", "0",
		"-hls_segment_filename", fmt.Sprintf("%s/segment_%%05d.ts", outputDir),
		playlistPath,
	}

	cmd := exec.Command("ffmpeg", cmdArgs...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	log.Printf("Starting LOW-LATENCY composite HLS streaming for participants: %s (%s) and %s (%s)",
		participant1ID, video1InputFormat, participant2ID, video2InputFormat)
	log.Printf("FFmpeg command: ffmpeg %v", cmdArgs)
	log.Printf("Input files: Video1: %s, Audio1: %s, Video2: %s, Audio2: %s",
		video1Path, audio1Path, video2Path, audio2Path)

	csm.hlsProcess = cmd
	csm.isStreaming = true
	csm.mutex.Unlock()

	err := cmd.Start()
	if err != nil {
		log.Printf("Error starting composite HLS FFmpeg process: %v", err)
		log.Printf("FFmpeg stderr at start: %s", stderrBuf.String())
		csm.mutex.Lock()
		csm.isStreaming = false
		csm.hlsProcess = nil
		csm.mutex.Unlock()
		return
	}
	log.Printf("LOW-LATENCY Composite HLS FFmpeg process started with PID: %d", cmd.Process.Pid)

	go func() {
		waitErr := cmd.Wait()
		csm.mutex.Lock()
		defer csm.mutex.Unlock()

		if csm.hlsProcess != cmd {
			log.Printf("FFmpeg process (PID: %d) finished, but a newer HLS process is active or none. Ignoring.", cmd.Process.Pid)
			return
		}

		if waitErr != nil {
			log.Printf("Composite HLS FFmpeg process (PID: %d) ended with error: %v", cmd.Process.Pid, waitErr)
			log.Printf("FFmpeg stderr output (PID: %d):\n%s", cmd.Process.Pid, stderrBuf.String())
			if exitError, ok := waitErr.(*exec.ExitError); ok {
				log.Printf("FFmpeg exit code (PID: %d): %d", cmd.Process.Pid, exitError.ExitCode())
			}
		} else {
			log.Printf("Composite HLS FFmpeg process (PID: %d) ended successfully.", cmd.Process.Pid)
		}
		csm.isStreaming = false
		csm.hlsProcess = nil
	}()
}

func (csm *CallSessionManager) removeParticipant(clientID string) {
	csm.mutex.Lock()
	defer csm.mutex.Unlock()

	log.Printf("Removing participant %s", clientID)

	delete(csm.participants, clientID)

	if writer, exists := csm.videoWriters[clientID]; exists {
		if err := writer.Close(); err != nil {
			log.Printf("Error closing video writer for %s: %v", clientID, err)
		}
		delete(csm.videoWriters, clientID)
		log.Printf("Closed and removed video writer for %s", clientID)
	}

	if writer, exists := csm.audioWriters[clientID]; exists {
		if err := writer.Close(); err != nil {
			log.Printf("Error closing audio writer for %s: %v", clientID, err)
		}
		delete(csm.audioWriters, clientID)
		log.Printf("Closed and removed audio writer for %s", clientID)
	}

	if file, exists := csm.videoFiles[clientID]; exists {
		filePath := file.Name()
		if err := file.Close(); err != nil {
			log.Printf("Error closing video file %s for %s: %v", filePath, clientID, err)
		}
		delete(csm.videoFiles, clientID)
		// os.Remove(filePath)
		log.Printf("Closed video file %s for %s", filePath, clientID)
	}

	if file, exists := csm.audioFiles[clientID]; exists {
		filePath := file.Name()
		if err := file.Close(); err != nil {
			log.Printf("Error closing audio file %s for %s: %v", filePath, clientID, err)
		}
		delete(csm.audioFiles, clientID)
		// os.Remove(filePath)
		log.Printf("Closed audio file %s for %s", filePath, clientID)
	}

	log.Printf("Participant %s removed. Remaining participants: %d, VideoWriters: %d, AudioWriters: %d",
		clientID, len(csm.participants), len(csm.videoWriters), len(csm.audioWriters))

	if len(csm.participants) < 2 || len(csm.videoWriters) < 2 || len(csm.audioWriters) < 2 {
		if csm.isStreaming && csm.hlsProcess != nil {
			log.Printf("Conditions for streaming no longer met after removing %s. Stopping HLS.", clientID)
			csm.stopCompositeHLSLocked()
		}
	}
}

func (csm *CallSessionManager) stopCompositeHLSLocked() {
	if csm.hlsProcess != nil && csm.hlsProcess.Process != nil {
		log.Printf("Stopping composite HLS process (PID: %d)", csm.hlsProcess.Process.Pid)
		if err := csm.hlsProcess.Process.Signal(os.Interrupt); err != nil {
			log.Printf("Failed to send SIGINT to FFmpeg process (PID: %d): %v. Attempting to kill.", csm.hlsProcess.Process.Pid, err)
			if killErr := csm.hlsProcess.Process.Kill(); killErr != nil {
				log.Printf("Failed to kill FFmpeg process (PID: %d): %v", csm.hlsProcess.Process.Pid, killErr)
			}
		}
	} else {
		log.Println("stopCompositeHLSLocked called, but no HLS process or process state found to stop.")
	}
}

func (csm *CallSessionManager) stopCompositeHLS() {
	csm.mutex.Lock()
	defer csm.mutex.Unlock()
	log.Println("stopCompositeHLS: Acquiring lock and calling stopCompositeHLSLocked.")
	csm.stopCompositeHLSLocked()
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

	p.mu.Lock()
	pc := p.peerConnection
	p.mu.Unlock()

	if pc == nil {
		if err := p.createServerPeerConnection(); err != nil {
			log.Printf("Peer %s: Error creating server peer connection for offer: %v", p.id, err)
			return
		}
		p.mu.Lock()
		pc = p.peerConnection
		p.mu.Unlock()
	}
	if pc == nil {
		log.Printf("Peer %s: Peer connection is nil even after creation attempt. Cannot handle server offer.", p.id)
		return
	}

	sdp := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  payload.SDP.SDP,
	}

	if err := pc.SetRemoteDescription(sdp); err != nil {
		log.Printf("Peer %s: Error setting remote description from client's offer: %v", p.id, err)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("Peer %s: Error creating answer to client's offer: %v", p.id, err)
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		log.Printf("Peer %s: Error setting local description for answer: %v", p.id, err)
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

	p.mu.Lock()
	pc := p.peerConnection
	p.mu.Unlock()

	if payload.Candidate == nil || pc == nil {
		log.Printf("Peer %s: Server candidate is nil or peer connection is nil. Skipping.", p.id)
		return
	}

	candidate := webrtc.ICECandidateInit{
		Candidate:        payload.Candidate.Candidate,
		SDPMid:           payload.Candidate.SDPMid,
		SDPMLineIndex:    payload.Candidate.SDPMLineIndex,
		UsernameFragment: payload.Candidate.UsernameFragment,
	}

	if pc.RemoteDescription() == nil {
		log.Printf("Peer %s: Remote description not set yet, cannot add ICE candidate. Client should queue or server should implement queuing.", p.id)
		return
	}

	if err := pc.AddICECandidate(candidate); err != nil {
		log.Printf("Peer %s: Error adding ICE candidate from client: %v", p.id, err)
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

	globalCallSession.mutex.RLock()
	defer globalCallSession.mutex.RUnlock()

	if msg.Type == "signal-initiate-p2p" {
		announcerID := p.id
		log.Printf("Peer %s is announcing 'signal-initiate-p2p'. Broadcasting to others and informing announcer about existing peers.", announcerID)

		var targets []*PeerConnectionContext
		var existingPeerIDs []string

		broadcastPayloadToOthers := PayloadWithFrom{FromPeerID: announcerID}
		marshaledBroadcastPayload, mErr := json.Marshal(broadcastPayloadToOthers)
		if mErr != nil {
			log.Printf("Peer %s: Error marshalling 'signal-initiate-p2p' broadcast payload to others: %v", announcerID, mErr)
			return
		}
		messageToBroadcastToOthers := Message{Type: "signal-initiate-p2p", Payload: marshaledBroadcastPayload}

		for peerID, existingPeerCtx := range globalCallSession.participants {
			if peerID == announcerID {
				continue
			}
			targets = append(targets, existingPeerCtx)
			existingPeerIDs = append(existingPeerIDs, peerID)
		}

		for _, targetCtx := range targets {
			log.Printf("Peer %s (announcer) signaling 'signal-initiate-p2p' to existing peer %s", announcerID, targetCtx.GetID())
			targetCtx.sendMessage(messageToBroadcastToOthers)
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

		if len(existingPeerIDs) == 0 {
			log.Printf("Peer %s (announcer): No other peers found during 'signal-initiate-p2p'.", announcerID)
		}
		return
	}

	targetPeerID := clientPayload.ToPeerID
	if targetPeerID == "" {
		log.Printf("Peer %s: P2P message type %s is missing 'toPeerID' in payload.", p.id, msg.Type)
		return
	}

	if targetCtx, exists := globalCallSession.participants[targetPeerID]; exists {
		log.Printf("Peer %s routing P2P message '%s' (from %s) to target peer %s", p.id, msg.Type, relayPayload.FromPeerID, targetPeerID)
		targetCtx.sendMessage(messageToRelay)
	} else {
		log.Printf("Peer %s: Target peer %s for P2P message type '%s' not found.", p.id, targetPeerID, msg.Type)
	}
}

func (p *PeerConnectionContext) HandleMessages() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Peer %s: Recovered in HandleMessages: %v", p.id, r)
		}
		p.Close()
	}()

	if err := p.createServerPeerConnection(); err != nil {
		log.Printf("Peer %s: Failed to create server peer connection on HandleMessages init: %v", p.id, err)
	}

	for {
		if p.isContextClosed() {
			log.Printf("Peer %s: Context is closed, exiting HandleMessages loop.", p.id)
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
			log.Printf("Peer %s: Unknown message type received: %s", p.id, msg.Type)
		}
	}
}

func (p *PeerConnectionContext) sendMessage(msg Message) {
	p.mu.Lock()
	wsRef := p.ws
	isCtxClosed := p.isClosed
	p.mu.Unlock()

	if isCtxClosed || wsRef == nil {
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
	peerConnRef := p.peerConnection
	p.peerConnection = nil
	p.mu.Unlock()

	globalCallSession.removeParticipant(p.id)

	if peerConnRef != nil {
		log.Printf("Peer %s: Closing server RTCPeerConnection.", p.id)
		if err := peerConnRef.Close(); err != nil {
			log.Printf("Peer %s: Error closing server RTCPeerConnection: %v", p.id, err)
		}
	}

	if wsRef != nil {
		log.Printf("Peer %s: Closing WebSocket connection.", p.id)
		if err := wsRef.Close(); err != nil {
			log.Printf("Peer %s: Error closing WebSocket in PeerConnectionContext.Close: %v", p.id, err)
		}
	}
	log.Printf("Peer %s: PeerConnectionContext closed.", p.id)
}

func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		log.Panicf("Failed to marshal JSON: %v", err)
	}
	return data
}
