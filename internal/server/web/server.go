// Package web implements the HTTP and WebSocket server for the kgpudash UI.
package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/MichaelTrip/kgpudash/internal/server/aggregator"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	// Allow all origins in development; restrict in production via config.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Server is the HTTP + WebSocket server.
type Server struct {
	log  *zap.Logger
	agg  *aggregator.Aggregator
	mux  *http.ServeMux
	http *http.Server
}

// New creates a new web Server.
func New(log *zap.Logger, agg *aggregator.Aggregator, addr string) *Server {
	s := &Server{
		log: log,
		agg: agg,
		mux: http.NewServeMux(),
	}
	s.registerRoutes()
	s.http = &http.Server{
		Addr:         addr,
		Handler:      s.mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // disabled for WebSocket
		IdleTimeout:  60 * time.Second,
	}
	return s
}

// registerRoutes wires up all HTTP routes.
func (s *Server) registerRoutes() {
	// Static assets embedded in the binary.
	s.mux.Handle("/", http.FileServer(http.FS(staticFS)))

	// Health check endpoint (used by Kubernetes probes).
	s.mux.HandleFunc("/healthz", s.handleHealth)

	// WebSocket endpoint for live metric updates.
	s.mux.HandleFunc("/ws", s.handleWebSocket)

	// REST endpoint for historical data (requires storage enabled).
	s.mux.HandleFunc("/api/history", s.handleHistory)
}

// Start begins serving HTTP. It blocks until the server is shut down.
func (s *Server) Start() error {
	s.log.Info("web server listening", zap.String("addr", s.http.Addr))
	return s.http.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// handleHealth responds to Kubernetes liveness/readiness probes.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// handleWebSocket upgrades the connection and streams metric snapshots.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("websocket upgrade failed", zap.Error(err))
		return
	}
	defer conn.Close()

	s.log.Info("websocket client connected", zap.String("remote", r.RemoteAddr))

	sub := s.agg.Subscribe()
	defer s.agg.Unsubscribe(sub)

	// Send the current full snapshot immediately on connect.
	if err := s.sendJSON(conn, s.agg.CurrentSnapshot()); err != nil {
		s.log.Warn("initial snapshot send failed", zap.Error(err))
		return
	}

	// Read pump: detect client disconnect.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	// Write pump: forward broadcasts to the client.
	for {
		select {
		case <-done:
			s.log.Info("websocket client disconnected", zap.String("remote", r.RemoteAddr))
			return
		case snap, ok := <-sub:
			if !ok {
				return
			}
			if err := s.sendJSON(conn, snap); err != nil {
				s.log.Warn("websocket send failed", zap.Error(err))
				return
			}
		}
	}
}

// handleHistory serves historical GPU metrics as JSON.
// Query params: node, gpu (index), from (unix ms), to (unix ms)
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	node := q.Get("node")
	gpuStr := q.Get("gpu")
	fromStr := q.Get("from")
	toStr := q.Get("to")

	if node == "" || gpuStr == "" {
		http.Error(w, "node and gpu params required", http.StatusBadRequest)
		return
	}

	gpuIndex, err := strconv.Atoi(gpuStr)
	if err != nil {
		http.Error(w, "invalid gpu index", http.StatusBadRequest)
		return
	}

	now := time.Now()
	from := now.Add(-1 * time.Hour)
	to := now

	if fromStr != "" {
		if ms, err := strconv.ParseInt(fromStr, 10, 64); err == nil {
			from = time.UnixMilli(ms)
		}
	}
	if toStr != "" {
		if ms, err := strconv.ParseInt(toStr, 10, 64); err == nil {
			to = time.UnixMilli(ms)
		}
	}

	points, err := s.agg.QueryHistory(node, gpuIndex, from, to)
	if err != nil {
		s.log.Warn("history query failed", zap.Error(err))
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	type historyResponse struct {
		Type   string      `json:"type"`
		Node   string      `json:"node"`
		GPU    int         `json:"gpu"`
		Points interface{} `json:"points"`
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(historyResponse{
		Type:   "history",
		Node:   node,
		GPU:    gpuIndex,
		Points: points,
	})
}

// sendJSON marshals v and sends it as a WebSocket text message.
func (s *Server) sendJSON(conn *websocket.Conn, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}
