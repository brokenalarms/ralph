package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// BDStub creates a CommandFunc that dispatches on bd subcommands and returns
// canned responses. Use it to test code that interacts with the bd CLI
// without spawning real processes or requiring a running Dolt server.
type BDStub struct {
	Total      string
	Counts     map[string]string // status -> count
	InProgress string            // JSON array for "list --status in_progress --json"
	Ready      string            // JSON array for "ready --json"
	ShowJSON   string            // JSON for "show <id> --json"
	ShowText   string            // plain text for "show <id>"
	Comments   string            // text for "comments <id>"
	Prime      string            // text for "prime"
	ListFlat   string            // text for "list --flat"
	ListClosed string            // text for "list --status closed"
	StateMap   map[string]string // "id:dimension" -> value
	Healthy    bool
	InitErr    error
	CloseErr   error
	UpdateErr  error

	calls []BDCall
}

// BDCall records a single bd command invocation.
type BDCall struct {
	Dir  string
	Args []string
}

// NewBDStub creates a BDStub with sensible defaults (healthy, 5 total, 3 open).
func NewBDStub() *BDStub {
	return &BDStub{
		Total:      "5",
		Counts:     map[string]string{"open": "3", "closed": "2", "in_progress": "0"},
		InProgress: "[]",
		Ready:      `[{"id":"abc123","title":"Fix the auth module"}]`,
		ShowJSON:   `[{"status":"in_progress"}]`,
		Healthy:    true,
		StateMap:   make(map[string]string),
	}
}

// Runner returns a CommandFunc suitable for injection into tasks.BD.RunBD.
func (s *BDStub) Runner() func(ctx context.Context, dir string, args ...string) (string, error) {
	return func(_ context.Context, dir string, args ...string) (string, error) {
		s.calls = append(s.calls, BDCall{Dir: dir, Args: args})

		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "init":
			return "", s.InitErr
		case "count":
			if !s.Healthy {
				return "", fmt.Errorf("server unreachable")
			}
			if len(args) >= 3 && args[1] == "--status" {
				if v, ok := s.Counts[args[2]]; ok {
					return v, nil
				}
				return "0", nil
			}
			return s.Total, nil
		case "list":
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "in_progress") && strings.Contains(joined, "--json") {
				return s.InProgress, nil
			}
			if strings.Contains(joined, "closed") {
				return s.ListClosed, nil
			}
			if strings.Contains(joined, "--flat") {
				return s.ListFlat, nil
			}
			return "[]", nil
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return s.Ready, nil
			}
			return "", nil
		case "show":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return s.ShowJSON, nil
			}
			return s.ShowText, nil
		case "close":
			return "closed", s.CloseErr
		case "update":
			return "", s.UpdateErr
		case "set-state":
			if len(args) >= 3 {
				// Parse "dimension=value" from args[2]
				parts := strings.SplitN(args[2], "=", 2)
				if len(parts) == 2 {
					key := args[1] + ":" + parts[0]
					s.StateMap[key] = parts[1]
				}
			}
			return "", nil
		case "state":
			if len(args) >= 3 {
				key := args[1] + ":" + args[2]
				if v, ok := s.StateMap[key]; ok {
					return v, nil
				}
			}
			return "", nil
		case "comments":
			return s.Comments, nil
		case "prime":
			return s.Prime, nil
		}
		return "", fmt.Errorf("unknown bd command: %s", args[0])
	}
}

// Calls returns all recorded bd invocations.
func (s *BDStub) Calls() []BDCall {
	return s.calls
}

// CalledWith returns true if any call matched the given arg prefix.
func (s *BDStub) CalledWith(args ...string) bool {
	for _, c := range s.calls {
		if len(c.Args) < len(args) {
			continue
		}
		match := true
		for i, a := range args {
			if c.Args[i] != a {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// MarshalIssues is a helper to create JSON issue arrays for Ready/InProgress fields.
func MarshalIssues(issues ...map[string]interface{}) string {
	data, _ := json.Marshal(issues)
	return string(data)
}
