package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/brokenalarms/ralph/internal/config"
)

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "ralph-server",
		"version": config.Version,
		"routes": []string{
			"POST /start   - Start a ralph loop",
			"GET  /status   - Get loop status",
			"POST /stop     - Request graceful stop",
			"POST /feedback - Send live feedback to the running agent",
			"POST /kill     - Kill the running process",
			"GET  /log      - Tail the loop log",
			"GET  /plan     - View the plan file",
			"DELETE /reset  - Clean .ralph state",
		},
	})
}

type startRequest struct {
	Dir          string `json:"dir"`
	Max          int    `json:"max"`
	Prompt       string `json:"prompt"`
	Resume       bool   `json:"resume"`
	PlanFile     string `json:"plan_file"`
	CallsPerHour int    `json:"calls_per_hour"`
	Tmux         bool   `json:"tmux"`
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.process != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "Loop already running",
			"project": s.projectDir,
		})
		return
	}

	var body startRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		body = startRequest{}
	}

	projectDir := body.Dir
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}
	maxIterations := body.Max
	if maxIterations == 0 {
		maxIterations = 50
	}

	if _, err := os.Stat(projectDir); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("Directory not found: %s", projectDir),
		})
		return
	}

	args := []string{s.ScriptPath, "loop", "--max", strconv.Itoa(maxIterations)}
	if body.Prompt != "" {
		args = append(args, "--prompt", body.Prompt)
	}
	if body.PlanFile != "" {
		args = append(args, "--plan-file", body.PlanFile)
	}
	if body.Resume {
		args = append(args, "--resume")
	}
	if body.CallsPerHour > 0 {
		args = append(args, "--calls-per-hour", strconv.Itoa(body.CallsPerHour))
	}
	if body.Tmux {
		args = append(args, "--tmux")
	}

	cmd := exec.Command("bash", args...)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("Failed to start: %s", err),
		})
		return
	}

	s.projectDir = projectDir
	s.planFile = body.PlanFile
	s.process = &Process{Cmd: cmd, PID: cmd.Process.Pid}

	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			}
		}
		log.Printf("[ralph-server] Loop exited with code %d", code)
		s.mu.Lock()
		s.process = nil
		s.planFile = ""
		s.mu.Unlock()
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "started",
		"pid":            cmd.Process.Pid,
		"project":        projectDir,
		"max_iterations": maxIterations,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	proc := s.process
	projectDir := s.projectDir
	planFile := s.planFile
	s.mu.Unlock()

	dir := projectDir
	if dir == "" {
		dir = r.Header.Get("X-Project-Dir")
	}
	if dir == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"running": false,
			"message": "No active loop",
		})
		return
	}

	rd := ralphDir(dir)
	state := readJSONFile(filepath.Join(rd, "state.json"))
	logTail := tailFile(filepath.Join(rd, "loop.log"), 30)
	planPath := resolvePlanPath(dir, rd, planFile)
	planContent := readTextFile(planPath)

	var planPreview *string
	if planContent != nil {
		s := *planContent
		if len(s) > 2000 {
			s = s[:2000]
		}
		planPreview = &s
	}

	var pid *int
	running := proc != nil
	if proc != nil {
		pid = &proc.PID
	}

	_, stopErr := os.Stat(filepath.Join(rd, "stop"))

	writeJSON(w, http.StatusOK, map[string]any{
		"running":          running,
		"pid":              pid,
		"project":          dir,
		"plan_file":        planFile,
		"state":            state,
		"log_tail":         logTail,
		"plan_preview":     planPreview,
		"stop_file_exists": stopErr == nil,
	})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	projectDir := s.projectDir
	s.mu.Unlock()

	if projectDir == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No active loop"})
		return
	}

	rd := ralphDir(projectDir)
	os.MkdirAll(rd, 0o755)
	content := fmt.Sprintf("stopped at %s\n", timeNow().UTC().Format("2006-01-02T15:04:05Z"))
	if err := os.WriteFile(filepath.Join(rd, "stop"), []byte(content), 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "stop_requested",
		"message": "Stop file created. Loop will halt after current iteration.",
	})
}

type feedbackRequest struct {
	Message string `json:"message"`
}

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	projectDir := s.projectDir
	s.mu.Unlock()

	if projectDir == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No active loop"})
		return
	}

	var body feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing 'message' field"})
		return
	}

	rd := ralphDir(projectDir)
	os.MkdirAll(rd, 0o755)

	taskID := readTaskIDFromStateFile(filepath.Join(rd, "state.json"))
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No active task"})
		return
	}

	appendFn := s.appendNotesDefault
	if s.AppendNotes != nil {
		appendFn = s.AppendNotes
	}
	if err := appendFn(projectDir, taskID, body.Message); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if err := os.WriteFile(filepath.Join(rd, "feedback"), nil, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "feedback_sent",
		"message": "Feedback appended to bead — agent will restart.",
	})
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	proc := s.process
	s.mu.Unlock()

	if proc == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No active process"})
		return
	}

	proc.Cmd.Process.Signal(syscall.SIGTERM)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "killed",
		"pid":    proc.PID,
	})
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	projectDir := s.projectDir
	s.mu.Unlock()

	dir := projectDir
	if dir == "" {
		dir = r.Header.Get("X-Project-Dir")
	}
	if dir == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No project directory"})
		return
	}

	logPath := filepath.Join(ralphDir(dir), "loop.log")
	lines := 100
	if q := r.URL.Query().Get("lines"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			lines = n
		}
	}

	content := tailFile(logPath, lines)
	if content == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No log file found"})
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write([]byte(*content))
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	projectDir := s.projectDir
	planFile := s.planFile
	s.mu.Unlock()

	dir := projectDir
	if dir == "" {
		dir = r.Header.Get("X-Project-Dir")
	}
	if dir == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No project directory"})
		return
	}

	planPath := resolvePlanPath(dir, ralphDir(dir), planFile)
	content := readTextFile(planPath)
	if content == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No plan file found"})
		return
	}

	w.Header().Set("Content-Type", "text/markdown")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write([]byte(*content))
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	proc := s.process
	projectDir := s.projectDir
	s.mu.Unlock()

	if proc != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Cannot reset while loop is running. Stop or kill first.",
		})
		return
	}

	dir := projectDir
	if dir == "" {
		dir = r.Header.Get("X-Project-Dir")
	}
	if dir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No project directory specified"})
		return
	}

	rd := ralphDir(dir)
	if base := filepath.Base(rd); base == ".beads" || base == ".dolt" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": fmt.Sprintf("refusing to remove %s: protected directory", base),
		})
		return
	}
	if _, err := os.Stat(rd); err == nil {
		if err := os.RemoveAll(rd); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	s.mu.Lock()
	s.projectDir = ""
	s.planFile = ""
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "reset",
		"message": ".ralph directory removed",
	})
}
