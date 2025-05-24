package main

import (
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

type CallSession struct {
	participants map[string]*PeerConnectionContext
	mutex        sync.RWMutex
	hlsProcess   *exec.Cmd
	videoWriters map[string]*ivfwriter.IVFWriter
	audioWriters map[string]*oggwriter.OggWriter
	videoFiles   map[string]*os.File
	audioFiles   map[string]*os.File
	isStreaming  bool
}

func NewCallSession() *CallSession {
	return &CallSession{
		participants: make(map[string]*PeerConnectionContext),
		videoWriters: make(map[string]*ivfwriter.IVFWriter),
		audioWriters: make(map[string]*oggwriter.OggWriter),
		videoFiles:   make(map[string]*os.File),
		audioFiles:   make(map[string]*os.File),
	}
}

func (cs *CallSession) AddParticipant(ws *websocket.Conn, clientID string) (*PeerConnectionContext, error) {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	if len(cs.participants) >= 2 {
		return nil, fmt.Errorf("call is full (max 2 participants)")
	}

	participant, err := NewPeerConnectionContext(ws, clientID)
	if err != nil {
		return nil, err
	}

	cs.participants[clientID] = participant
	log.Printf("Participant %s added to call. Total participants: %d", clientID, len(cs.participants))

	return participant, nil
}

func (cs *CallSession) RemoveParticipant(clientID string) {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	if participant, exists := cs.participants[clientID]; exists {
		participant.Close()
		delete(cs.participants, clientID)
		log.Printf("Participant %s removed from call. Total participants: %d", clientID, len(cs.participants))

		if len(cs.participants) == 0 {
			cs.stopCompositeHLS()
		}
	}
}

func (cs *CallSession) IsEmpty() bool {
	cs.mutex.RLock()
	defer cs.mutex.RUnlock()
	return len(cs.participants) == 0
}

func (cs *CallSession) OnTrackReceived(clientID string, track *webrtc.TrackRemote) {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	outputDir := "./hls-output"
	os.MkdirAll(outputDir, 0755)

	if track.Kind() == webrtc.RTPCodecTypeVideo {
		videoFilePath := fmt.Sprintf("%s/video_%s.ivf", outputDir, clientID)
		videoFile, err := os.Create(videoFilePath)
		if err != nil {
			log.Printf("Error creating video file for %s: %v", clientID, err)
			return
		}

		cs.videoFiles[clientID] = videoFile

		videoWriter, err := ivfwriter.New(videoFilePath)
		if err != nil {
			log.Printf("Error creating video writer for %s: %v", clientID, err)
			return
		}

		cs.videoWriters[clientID] = videoWriter
		go cs.saveVideoTrack(clientID, track)

	} else if track.Kind() == webrtc.RTPCodecTypeAudio {
		audioFilePath := fmt.Sprintf("%s/audio_%s.ogg", outputDir, clientID)
		audioFile, err := os.Create(audioFilePath)
		if err != nil {
			log.Printf("Error creating audio file for %s: %v", clientID, err)
			return
		}

		cs.audioFiles[clientID] = audioFile

		audioWriter, err := oggwriter.New(audioFilePath, 48000, 2)
		if err != nil {
			log.Printf("Error creating audio writer for %s: %v", clientID, err)
			return
		}

		cs.audioWriters[clientID] = audioWriter
		go cs.saveAudioTrack(clientID, track)
	}

	if len(cs.videoWriters) == 2 && !cs.isStreaming {
		go func() {
			time.Sleep(3 * time.Second)
			cs.startCompositeHLS()
		}()
	}
}

func (cs *CallSession) saveVideoTrack(clientID string, track *webrtc.TrackRemote) {
	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			log.Printf("Error reading video RTP for %s: %v", clientID, err)
			return
		}

		if writer := cs.videoWriters[clientID]; writer != nil {
			err = writer.WriteRTP(rtpPacket)
			if err != nil {
				log.Printf("Error writing video RTP for %s: %v", clientID, err)
				return
			}
		}
	}
}

func (cs *CallSession) saveAudioTrack(clientID string, track *webrtc.TrackRemote) {
	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			log.Printf("Error reading audio RTP for %s: %v", clientID, err)
			return
		}

		if writer := cs.audioWriters[clientID]; writer != nil {
			err = writer.WriteRTP(rtpPacket)
			if err != nil {
				log.Printf("Error writing audio RTP for %s: %v", clientID, err)
				return
			}
		}
	}
}

func (cs *CallSession) startCompositeHLS() {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	if cs.isStreaming || len(cs.videoWriters) < 2 {
		return
	}

	outputDir := "./hls-output"
	playlistPath := fmt.Sprintf("%s/playlist.m3u8", outputDir)

	var participant1, participant2 string
	i := 0
	for clientID := range cs.videoWriters {
		if i == 0 {
			participant1 = clientID
		} else {
			participant2 = clientID
		}
		i++
	}

	video1Path := fmt.Sprintf("%s/video_%s.ivf", outputDir, participant1)
	audio1Path := fmt.Sprintf("%s/audio_%s.ogg", outputDir, participant1)
	video2Path := fmt.Sprintf("%s/video_%s.ivf", outputDir, participant2)
	audio2Path := fmt.Sprintf("%s/audio_%s.ogg", outputDir, participant2)

	cmd := exec.Command("ffmpeg",
		"-re",
		"-i", video1Path,
		"-i", audio1Path,
		"-i", video2Path,
		"-i", audio2Path,
		"-filter_complex",
		"[0:v]scale=960:720[left];[2:v]scale=960:720[right];[left][right]hstack=inputs=2:shortest=1[v];[1:a][3:a]amix=inputs=2[a]",
		"-map", "[v]",
		"-map", "[a]",
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

	log.Printf("Starting composite HLS streaming")
	cs.hlsProcess = cmd
	cs.isStreaming = true

	err := cmd.Start()
	if err != nil {
		log.Printf("Error starting composite HLS: %v", err)
		cs.isStreaming = false
		return
	}

	go func() {
		err := cmd.Wait()
		if err != nil {
			log.Printf("Composite HLS process ended with error: %v", err)
		} else {
			log.Printf("Composite HLS process ended successfully")
		}
		cs.isStreaming = false
	}()
}

func (cs *CallSession) stopCompositeHLS() {
	if cs.hlsProcess != nil {
		log.Printf("Stopping composite HLS streaming")
		cs.hlsProcess.Process.Kill()
		cs.hlsProcess = nil
		cs.isStreaming = false
	}

	for clientID, writer := range cs.videoWriters {
		writer.Close()
		delete(cs.videoWriters, clientID)
	}

	for clientID, writer := range cs.audioWriters {
		writer.Close()
		delete(cs.audioWriters, clientID)
	}

	for clientID, file := range cs.videoFiles {
		file.Close()
		delete(cs.videoFiles, clientID)
	}

	for clientID, file := range cs.audioFiles {
		file.Close()
		delete(cs.audioFiles, clientID)
	}
}

func (cs *CallSession) Close() {
	cs.stopCompositeHLS()

	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	for _, participant := range cs.participants {
		participant.Close()
	}
	cs.participants = make(map[string]*PeerConnectionContext)
}
