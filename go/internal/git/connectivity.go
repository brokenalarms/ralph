package git

import (
	"net"
	"strings"
	"time"
)

// IsTransientGitHubError returns true for HTTP errors that are safe to retry:
// 401 Unauthorized (token briefly invalid) and 5xx server errors.
func IsTransientGitHubError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{"401", "500", "502", "503", "504"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// IsOnline checks internet connectivity with a quick TCP dial.
func IsOnline(timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", "api.anthropic.com:443", timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
