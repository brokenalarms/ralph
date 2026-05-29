package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(data)
}

func ralphDir(projectDir string) string {
	return filepath.Join(projectDir, ".ralph")
}

func resolvePlanPath(projectDir, rd, planFile string) string {
	if planFile == "" {
		return filepath.Join(rd, "plan.md")
	}
	if filepath.IsAbs(planFile) {
		return planFile
	}
	return filepath.Join(projectDir, planFile)
}

func readJSONFile(path string) any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var result any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}
	return result
}

func readTextFile(path string) *string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	s := string(data)
	return &s
}

func tailFile(path string, lines int) *string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)
	allLines := strings.Split(content, "\n")
	if len(allLines) > lines {
		allLines = allLines[len(allLines)-lines:]
	}
	result := strings.Join(allLines, "\n")
	return &result
}

func (s *Server) appendNotesDefault(projectDir, taskID, msg string) error {
	bdBin, err := findBDBin()
	if err != nil {
		return err
	}
	cmd := exec.Command(bdBin, "update", taskID, "--append-notes", msg)
	cmd.Dir = projectDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func readTaskIDFromStateFile(statePath string) string {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return ""
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return ""
	}
	v, ok := raw["current_task_id"]
	if !ok {
		// Backwards compat: old state files written by bash ralph use last_task_id.
		v, ok = raw["last_task_id"]
		if !ok {
			return ""
		}
	}
	var id string
	if json.Unmarshal(v, &id) != nil {
		return ""
	}
	return id
}

func findBDBin() (string, error) {
	if p, err := exec.LookPath("bd"); err == nil {
		return p, nil
	}
	if home, _ := os.UserHomeDir(); home != "" {
		p := filepath.Join(home, ".local", "bin", "bd")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("bd binary not found")
}
