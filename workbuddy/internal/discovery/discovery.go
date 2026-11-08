// Package discovery locates the loopback ACP endpoint of a running WorkBuddy
// PC client from the per-session JSON files under ~/.workbuddy/sessions/.
//
// Ports are recycled when a session returns to the prewarm pool, so the
// directory is re-read on every bridge start; no port is cached.
package discovery

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultSessionsDir is the directory published by the WorkBuddy PC client.
func DefaultSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".workbuddy", "sessions")
}

var loopbackHosts = map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true}

// IsLoopbackEndpoint accepts only plain http://127.0.0.1:<port> URLs
// (or localhost / ::1), with no path, query, fragment or user info.
func IsLoopbackEndpoint(raw string) bool {
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if !loopbackHosts[host] {
		return false
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	if strings.Trim(parsed.Path, "/") != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return true
}

// Session is one live interactive session record.
type Session struct {
	Pid           int    `json:"pid"`
	SessionID     string `json:"sessionId"`
	Cwd           string `json:"cwd"`
	Kind          string `json:"kind"`
	Endpoint      string `json:"endpoint"`
	Version       string `json:"version"`
	Mode          string `json:"mode"`
	LastHeartbeat any    `json:"lastHeartbeat"`
	File          string `json:"-"`
	Explicit      bool   `json:"-"`
}

// Heartbeat normalizes the mixed int/float/string forms found in session files.
func (s Session) Heartbeat() int64 {
	switch value := s.LastHeartbeat.(type) {
	case float64:
		return int64(value)
	case int:
		return int64(value)
	case int64:
		return value
	case json.Number:
		number, _ := value.Int64()
		return number
	case string:
		number, _ := strconv.ParseInt(value, 10, 64)
		return number
	default:
		return 0
	}
}

// ReadSessions returns live interactive sessions ordered by heartbeat.
// Missing directories, unreadable files and malformed records are skipped.
func ReadSessions(directory string) []Session {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return []Session{}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	sessions := make([]Session, 0, len(names))
	for _, name := range names {
		path := filepath.Join(directory, name)
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(body, &record); err != nil || record["kind"] != "interactive" {
			continue
		}
		raw, _ := record["url"].(string)
		if raw == "" {
			raw, _ = record["endpoint"].(string)
		}
		if !IsLoopbackEndpoint(raw) {
			continue
		}
		session := Session{
			Endpoint:      strings.TrimRight(raw, "/"),
			File:          path,
			LastHeartbeat: record["lastHeartbeat"],
		}
		if value, ok := record["pid"].(float64); ok {
			session.Pid = int(value)
		}
		session.SessionID, _ = record["sessionId"].(string)
		session.Cwd, _ = record["cwd"].(string)
		session.Kind, _ = record["kind"].(string)
		session.Version, _ = record["version"].(string)
		session.Mode, _ = record["mode"].(string)
		sessions = append(sessions, session)
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].Heartbeat() > sessions[j].Heartbeat()
	})
	return sessions
}

// Options controls session selection.
type Options struct {
	SessionID string
	Cwd       string
	Endpoint  string
	Directory string
}

// Select returns the session record to use, or a descriptive error.
func Select(options Options) (Session, error) {
	if options.Endpoint != "" {
		if !IsLoopbackEndpoint(options.Endpoint) {
			return Session{}, errors.New("endpoint must be a plain http://127.0.0.1:<port> URL")
		}
		return Session{
			Endpoint:  strings.TrimRight(options.Endpoint, "/"),
			SessionID: options.SessionID,
			Cwd:       options.Cwd,
			Explicit:  true,
		}, nil
	}
	sessions := ReadSessions(options.Directory)
	if len(sessions) == 0 {
		return Session{}, errors.New("no live interactive WorkBuddy session was found")
	}
	if options.SessionID != "" {
		for _, session := range sessions {
			if session.SessionID == options.SessionID {
				return session, nil
			}
		}
		return Session{}, errors.New("session " + options.SessionID + " has no live loopback endpoint")
	}
	if options.Cwd != "" {
		target := resolvePath(options.Cwd)
		for _, session := range sessions {
			if session.Cwd != "" && resolvePath(session.Cwd) == target {
				return session, nil
			}
		}
		return Session{}, errors.New("no live interactive session for cwd " + target)
	}
	return sessions[0], nil
}

func resolvePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return filepath.Clean(absolute)
}
