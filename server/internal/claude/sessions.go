package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Claude Code stores sessions in ~/.claude/projects/<dir with non-alphanumerics as '->/<id>.jsonl,
// so sessions started in the terminal and in the app are the same sessions.

type SessionInfo struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Message struct {
	Role      string    `json:"role"` // user | assistant
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp"`
}

func ProjectDir(moduleDir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	enc := strings.Map(func(r rune) rune {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, moduleDir)
	return filepath.Join(home, ".claude", "projects", enc), nil
}

func ListSessions(moduleDir string) ([]SessionInfo, error) {
	dir, err := ProjectDir(moduleDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []SessionInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []SessionInfo{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if e.IsDir() || !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, SessionInfo{ID: id, Title: sessionTitle(filepath.Join(dir, e.Name())), UpdatedAt: fi.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// SessionMessages returns the user/assistant text turns of a session.
func SessionMessages(moduleDir, sessionID string) ([]Message, error) {
	if !validSessionID(sessionID) {
		return nil, os.ErrNotExist
	}
	dir, err := ProjectDir(moduleDir)
	if err != nil {
		return nil, err
	}
	out := []Message{}
	err = eachLine(filepath.Join(dir, sessionID+".jsonl"), func(e *entry) bool {
		if text := e.text(); text != "" {
			out = append(out, Message{Role: e.Message.Role, Text: text, Timestamp: e.Timestamp})
		}
		return true
	})
	return out, err
}

func validSessionID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func sessionTitle(path string) string {
	var title string
	_ = eachLine(path, func(e *entry) bool {
		if e.Type == "user" {
			if t := e.text(); t != "" {
				title = t
				return false
			}
		}
		return true
	})
	if r := []rune(title); len(r) > 120 {
		title = string(r[:120]) + "…"
	}
	return title
}

type entry struct {
	Type      string    `json:"type"`
	IsMeta    bool      `json:"isMeta"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// text extracts human-visible text, skipping tool calls/results and injected
// system content (which starts with '<').
func (e *entry) text() string {
	if e.IsMeta || (e.Type != "user" && e.Type != "assistant") {
		return ""
	}
	var s string
	if json.Unmarshal(e.Message.Content, &s) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return ""
		}
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		s = strings.Join(parts, "\n")
	}
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<") {
		return ""
	}
	return s
}

// eachLine streams a JSONL file without a line-length cap (lines can be MBs).
func eachLine(path string, fn func(*entry) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var e entry
			if json.Unmarshal(line, &e) == nil && !fn(&e) {
				return nil
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
