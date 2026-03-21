package server

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	s := New("127.0.0.1", 0, "/bin/echo")
	return s, dir
}

// Verify the index route returns server metadata and available routes
// so clients can discover the API.
func TestIndexRoute(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	s.handleIndex(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["name"] != "ralph-server" {
		t.Fatalf("expected name ralph-server, got %v", resp["name"])
	}
	routes := resp["routes"].([]any)
	if len(routes) != 8 {
		t.Fatalf("expected 8 routes, got %d", len(routes))
	}
}

// Verify unknown paths return 404, not a fallback to the index page.
func TestIndexRoute404(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/nonexistent", nil)
	w := httptest.NewRecorder()
	s.handleIndex(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// Verify that /start rejects a second concurrent loop, returning 409
// with the project dir of the existing run.
func TestStartConflict(t *testing.T) {
	s, _ := newTestServer(t)
	s.process = &Process{PID: 1234}
	s.projectDir = "/some/dir"

	body := `{"dir": "/tmp"}`
	req := httptest.NewRequest("POST", "/start", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleStart(w, req)

	if w.Code != 409 {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// Verify that /start rejects a nonexistent directory with 400.
func TestStartBadDir(t *testing.T) {
	s, _ := newTestServer(t)

	body := `{"dir": "/nonexistent/path/that/doesnt/exist"}`
	req := httptest.NewRequest("POST", "/start", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleStart(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// Verify /status returns running=false with an explanatory message
// when no loop has been started.
func TestStatusNoLoop(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	s.handleStatus(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["running"] != false {
		t.Fatal("expected running=false")
	}
}

// Verify /status reads state.json and reports the stop file presence
// when a project dir is active, proving the status endpoint aggregates
// ralph's file-based state correctly.
func TestStatusWithProject(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir

	rd := filepath.Join(dir, ".ralph")
	os.MkdirAll(rd, 0o755)
	os.WriteFile(filepath.Join(rd, "state.json"), []byte(`{"status":"running","iteration":3}`), 0o644)
	os.WriteFile(filepath.Join(rd, "stop"), []byte("stopped"), 0o644)

	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	s.handleStatus(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["stop_file_exists"] != true {
		t.Fatal("expected stop_file_exists=true")
	}
	state := resp["state"].(map[string]any)
	if state["status"] != "running" {
		t.Fatalf("expected state.status=running, got %v", state["status"])
	}
}

// Verify /status falls back to X-Project-Dir header when no active loop,
// allowing clients to query status for a specific project.
func TestStatusFromHeader(t *testing.T) {
	s, dir := newTestServer(t)
	rd := filepath.Join(dir, ".ralph")
	os.MkdirAll(rd, 0o755)
	os.WriteFile(filepath.Join(rd, "state.json"), []byte(`{"iteration":5}`), 0o644)

	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("X-Project-Dir", dir)
	w := httptest.NewRecorder()
	s.handleStatus(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["project"] != dir {
		t.Fatalf("expected project=%s, got %v", dir, resp["project"])
	}
}

// Verify /stop creates the stop sentinel file that ralph.sh checks
// to gracefully halt after the current iteration.
func TestStop(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir

	origTimeNow := timeNow
	timeNow = func() time.Time { return time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC) }
	defer func() { timeNow = origTimeNow }()

	req := httptest.NewRequest("POST", "/stop", nil)
	w := httptest.NewRecorder()
	s.handleStop(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	stopPath := filepath.Join(dir, ".ralph", "stop")
	data, err := os.ReadFile(stopPath)
	if err != nil {
		t.Fatalf("stop file not created: %v", err)
	}
	if !strings.Contains(string(data), "2026-03-20") {
		t.Fatalf("stop file missing timestamp: %s", data)
	}
}

// Verify /stop returns 404 when no loop is active.
func TestStopNoLoop(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/stop", nil)
	w := httptest.NewRecorder()
	s.handleStop(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// Verify /feedback appends the message to the feedback file,
// which ralph.sh reads and injects into the next iteration's prompt.
func TestFeedback(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir

	body := `{"message": "please fix the tests"}`
	req := httptest.NewRequest("POST", "/feedback", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleFeedback(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	feedbackPath := filepath.Join(dir, ".ralph", "feedback")
	data, err := os.ReadFile(feedbackPath)
	if err != nil {
		t.Fatalf("feedback file not created: %v", err)
	}
	if !strings.Contains(string(data), "please fix the tests") {
		t.Fatalf("feedback content wrong: %s", data)
	}
}

// Verify /feedback appends (not overwrites) so multiple feedback
// messages queue up for the next iteration.
func TestFeedbackAppends(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir

	for _, msg := range []string{"first", "second"} {
		body := `{"message": "` + msg + `"}`
		req := httptest.NewRequest("POST", "/feedback", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleFeedback(w, req)
		if w.Code != 200 {
			t.Fatalf("expected 200 for %s, got %d", msg, w.Code)
		}
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".ralph", "feedback"))
	if !strings.Contains(string(data), "first") || !strings.Contains(string(data), "second") {
		t.Fatalf("expected both messages, got: %s", data)
	}
}

// Verify /feedback requires the message field.
func TestFeedbackMissingMessage(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir

	req := httptest.NewRequest("POST", "/feedback", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.handleFeedback(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// Verify /kill returns 404 when no process is running.
func TestKillNoProcess(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/kill", nil)
	w := httptest.NewRecorder()
	s.handleKill(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// Verify /log returns the tail of the loop log file, proving the
// endpoint correctly reads and truncates the log for remote monitoring.
func TestLog(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir

	rd := filepath.Join(dir, ".ralph")
	os.MkdirAll(rd, 0o755)
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = strings.Repeat("x", 10)
	}
	os.WriteFile(filepath.Join(rd, "loop.log"), []byte(strings.Join(lines, "\n")), 0o644)

	req := httptest.NewRequest("GET", "/log?lines=5", nil)
	w := httptest.NewRecorder()
	s.handleLog(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	resultLines := strings.Split(string(body), "\n")
	if len(resultLines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(resultLines))
	}
}

// Verify /log returns 404 when no log file exists.
func TestLogNotFound(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir

	req := httptest.NewRequest("GET", "/log", nil)
	w := httptest.NewRecorder()
	s.handleLog(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// Verify /plan returns the plan file content with text/markdown content type.
func TestPlan(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir

	rd := filepath.Join(dir, ".ralph")
	os.MkdirAll(rd, 0o755)
	os.WriteFile(filepath.Join(rd, "plan.md"), []byte("# My Plan\n- step 1"), 0o644)

	req := httptest.NewRequest("GET", "/plan", nil)
	w := httptest.NewRecorder()
	s.handlePlan(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/markdown" {
		t.Fatalf("expected text/markdown, got %s", ct)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "# My Plan") {
		t.Fatalf("plan content wrong: %s", body)
	}
}

// Verify /plan resolves a custom plan_file path relative to the project dir.
func TestPlanCustomFile(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir
	s.planFile = "custom-plan.md"

	os.WriteFile(filepath.Join(dir, "custom-plan.md"), []byte("custom plan"), 0o644)

	req := httptest.NewRequest("GET", "/plan", nil)
	w := httptest.NewRecorder()
	s.handlePlan(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if string(body) != "custom plan" {
		t.Fatalf("expected custom plan, got %s", body)
	}
}

// Verify /reset removes the .ralph directory and clears server state,
// allowing a fresh start.
func TestReset(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir

	rd := filepath.Join(dir, ".ralph")
	os.MkdirAll(rd, 0o755)
	os.WriteFile(filepath.Join(rd, "state.json"), []byte("{}"), 0o644)

	req := httptest.NewRequest("DELETE", "/reset", nil)
	w := httptest.NewRecorder()
	s.handleReset(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if _, err := os.Stat(rd); !os.IsNotExist(err) {
		t.Fatal("expected .ralph dir to be removed")
	}

	s.mu.Lock()
	if s.projectDir != "" {
		t.Fatal("expected projectDir to be cleared")
	}
	s.mu.Unlock()
}

// Verify /reset refuses to run while a loop is active, preventing
// data loss mid-iteration.
func TestResetWhileRunning(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir
	s.process = &Process{PID: 1234}

	req := httptest.NewRequest("DELETE", "/reset", nil)
	w := httptest.NewRecorder()
	s.handleReset(w, req)

	if w.Code != 409 {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// Verify /reset only removes .ralph state, leaving .beads (permanent
// task history) intact.
func TestResetPreservesBeads(t *testing.T) {
	s, dir := newTestServer(t)
	s.projectDir = dir

	rd := filepath.Join(dir, ".ralph")
	os.MkdirAll(rd, 0o755)
	os.WriteFile(filepath.Join(rd, "state.json"), []byte("{}"), 0o644)

	beadsDir := filepath.Join(dir, ".beads")
	os.MkdirAll(beadsDir, 0o755)
	os.WriteFile(filepath.Join(beadsDir, "data.db"), []byte("tasks"), 0o644)

	req := httptest.NewRequest("DELETE", "/reset", nil)
	w := httptest.NewRecorder()
	s.handleReset(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if _, err := os.Stat(rd); !os.IsNotExist(err) {
		t.Fatal("expected .ralph to be removed")
	}
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		t.Fatal(".beads must survive a reset")
	}
}

// Verify CORS preflight returns proper headers so browser-based
// clients (dashboard) can make cross-origin requests.
func TestCORSPreflight(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("OPTIONS", "/start", nil)
	w := httptest.NewRecorder()
	s.handleCORS(w, req)

	if w.Code != 204 {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("missing CORS origin header")
	}
}

// Verify JSON responses include CORS headers on every response,
// not just preflight.
func TestJSONResponsesHaveCORS(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	s.handleStatus(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("JSON response missing CORS header")
	}
}

// Verify tailFile correctly returns only the last N lines.
func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.log")
	lines := []string{"line1", "line2", "line3", "line4", "line5"}
	os.WriteFile(f, []byte(strings.Join(lines, "\n")), 0o644)

	result := tailFile(f, 3)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	parts := strings.Split(*result, "\n")
	if len(parts) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(parts), parts)
	}
	if parts[0] != "line3" {
		t.Fatalf("expected line3, got %s", parts[0])
	}
}

// Verify tailFile returns nil for nonexistent files.
func TestTailFileNotFound(t *testing.T) {
	result := tailFile("/nonexistent/path", 10)
	if result != nil {
		t.Fatal("expected nil for nonexistent file")
	}
}

// Verify resolvePlanPath picks the right path for default, relative,
// and absolute plan file references.
func TestResolvePlanPath(t *testing.T) {
	tests := []struct {
		name     string
		planFile string
		want     string
	}{
		{"default", "", "/project/.ralph/plan.md"},
		{"relative", "my-plan.md", "/project/my-plan.md"},
		{"absolute", "/tmp/plan.md", "/tmp/plan.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePlanPath("/project", "/project/.ralph", tt.planFile)
			if got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}
