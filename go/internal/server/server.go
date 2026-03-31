package server

import (
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

var timeNow = time.Now

type Server struct {
	Host       string
	Port       int
	ScriptPath string

	// AppendNotes appends feedback to a bead's notes. When nil,
	// defaults to shelling out to bd.
	AppendNotes func(projectDir, taskID, msg string) error

	mu         sync.Mutex
	process    *Process
	projectDir string
	planFile   string
	httpServer *http.Server
}

type Process struct {
	Cmd *exec.Cmd
	PID int
}

func New(host string, port int, scriptPath string) *Server {
	return &Server{
		Host:       host,
		Port:       port,
		ScriptPath: scriptPath,
	}
}

func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("POST /start", s.handleStart)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /stop", s.handleStop)
	mux.HandleFunc("POST /feedback", s.handleFeedback)
	mux.HandleFunc("POST /kill", s.handleKill)
	mux.HandleFunc("GET /log", s.handleLog)
	mux.HandleFunc("GET /plan", s.handlePlan)
	mux.HandleFunc("DELETE /reset", s.handleReset)
	mux.HandleFunc("OPTIONS /", s.handleCORS)

	addr := fmt.Sprintf("%s:%d", s.Host, s.Port)
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: corsMiddleware(mux),
	}

	log.Printf("[ralph-server] Listening on http://%s", addr)
	log.Printf("[ralph-server] Ralph script: %s", s.ScriptPath)
	return s.httpServer.ListenAndServe()
}

func (s *Server) handleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Project-Dir")
	w.WriteHeader(http.StatusNoContent)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Project-Dir")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
