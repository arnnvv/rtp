package main

import (
	"context"
	"github.com/gorilla/websocket"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	streamerWsPathDefault = "/ws/stream"
	serverAddrDefault     = ":8080"
	serverShutdownTimeout = 5 * time.Second
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	currentCall *CallSession
	callMutex   sync.RWMutex
)

func main() {
	os.MkdirAll("./hls-output", 0755)

	http.HandleFunc(streamerWsPathDefault, handleStreamerConnections)
	http.HandleFunc("/hls/", handleHLSRequest) // No clientId needed
	http.Handle("/", http.FileServer(http.Dir("./static/")))

	log.Printf("Server starting on %s...", serverAddrDefault)
	httpServer := &http.Server{Addr: serverAddrDefault}

	go func() {
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down server...")

	callMutex.Lock()
	if currentCall != nil {
		currentCall.Close()
		currentCall = nil
	}
	callMutex.Unlock()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	log.Println("Server gracefully stopped.")
}

func handleStreamerConnections(w http.ResponseWriter, r *http.Request) {
	clientIDs, ok := r.URL.Query()["clientId"]
	if !ok || len(clientIDs[0]) < 1 {
		log.Printf("Connection rejected: clientId query parameter is missing")
		http.Error(w, "clientId query parameter is required", http.StatusBadRequest)
		return
	}

	clientID := clientIDs[0]
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection for client %s: %v", clientID, err)
		return
	}

	log.Printf("Client connected: %s", clientID)

	callMutex.Lock()
	if currentCall == nil {
		currentCall = NewCallSession()
	}
	participant, err := currentCall.AddParticipant(ws, clientID)
	callMutex.Unlock()

	if err != nil {
		log.Printf("Failed to add participant %s: %v", clientID, err)
		ws.Close()
		return
	}

	go participant.HandleMessages()

	ws.SetCloseHandler(func(code int, text string) error {
		log.Printf("Client %s disconnected", clientID)
		callMutex.Lock()
		if currentCall != nil {
			currentCall.RemoveParticipant(clientID)
			if currentCall.IsEmpty() {
				currentCall.Close()
				currentCall = nil
			}
		}
		callMutex.Unlock()
		return nil
	})
}

func handleHLSRequest(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[5:] // Remove "/hls/" prefix

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if len(path) > 5 && path[len(path)-5:] == ".m3u8" {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	} else if len(path) > 3 && path[len(path)-3:] == ".ts" {
		w.Header().Set("Content-Type", "video/mp2t")
	}

	filePath := "./hls-output/" + path
	http.ServeFile(w, r, filePath)
}
