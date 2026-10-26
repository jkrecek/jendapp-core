// Package store is the SQLite persistence layer.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS devices (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created_at TIMESTAMP NOT NULL,
	last_seen  TIMESTAMP
);
CREATE TABLE IF NOT EXISTS inbox (
	id         TEXT PRIMARY KEY,
	filename   TEXT NOT NULL,
	path       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	status     TEXT NOT NULL,          -- pending | routed | skipped | failed
	module_id  TEXT,
	dest_path  TEXT,
	error      TEXT,
	created_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
	id          TEXT PRIMARY KEY,
	module_id   TEXT NOT NULL,
	kind        TEXT NOT NULL,         -- chat | import
	session_id  TEXT NOT NULL,
	status      TEXT NOT NULL,         -- queued | running | done | failed
	prompt      TEXT NOT NULL,
	result      TEXT,
	error       TEXT,
	cost_usd    REAL,
	denials     TEXT,                  -- JSON array of denied tool calls
	created_at  TIMESTAMP NOT NULL,
	finished_at TIMESTAMP
);
`

func Open(dataDir string) (*Store, error) {
	dsn := filepath.Join(dataDir, "jendapp.db") + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- devices ---

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// CreateDevice returns the plaintext token; only its hash is stored.
func (s *Store) CreateDevice(name string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	_, err := s.db.Exec(`INSERT INTO devices (name, token_hash, created_at) VALUES (?, ?, ?)`,
		name, hashToken(token), time.Now().UTC())
	return token, err
}

func (s *Store) AuthDevice(token string) (string, bool) {
	var id int64
	var name string
	err := s.db.QueryRow(`SELECT id, name FROM devices WHERE token_hash = ?`, hashToken(token)).Scan(&id, &name)
	if err != nil {
		return "", false
	}
	_, _ = s.db.Exec(`UPDATE devices SET last_seen = ? WHERE id = ?`, time.Now().UTC(), id)
	return name, true
}

type Device struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	LastSeen  *time.Time `json:"last_seen"`
}

func (s *Store) Devices() ([]Device, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at, last_seen FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		var last sql.NullTime
		if err := rows.Scan(&d.ID, &d.Name, &d.CreatedAt, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			d.LastSeen = &last.Time
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) RevokeDevice(id int64) error {
	res, err := s.db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- inbox ---

var ErrNotFound = errors.New("not found")

type InboxItem struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	Path      string    `json:"-"`
	Size      int64     `json:"size"`
	Status    string    `json:"status"`
	ModuleID  *string   `json:"module_id"`
	DestPath  *string   `json:"dest_path"`
	Error     *string   `json:"error"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) InsertInbox(it *InboxItem) error {
	_, err := s.db.Exec(`INSERT INTO inbox (id, filename, path, size, status, module_id, dest_path, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.ID, it.Filename, it.Path, it.Size, it.Status, it.ModuleID, it.DestPath, it.Error, it.CreatedAt)
	return err
}

func (s *Store) UpdateInbox(it *InboxItem) error {
	_, err := s.db.Exec(`UPDATE inbox SET status = ?, module_id = ?, dest_path = ?, error = ? WHERE id = ?`,
		it.Status, it.ModuleID, it.DestPath, it.Error, it.ID)
	return err
}

func (s *Store) GetInbox(id string) (*InboxItem, error) {
	items, err := s.queryInbox(`WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return &items[0], nil
}

// ListInbox returns newest first; status "" means all.
func (s *Store) ListInbox(status string, limit int) ([]InboxItem, error) {
	if status == "" {
		return s.queryInbox(`ORDER BY created_at DESC LIMIT ?`, limit)
	}
	return s.queryInbox(`WHERE status = ? ORDER BY created_at DESC LIMIT ?`, status, limit)
}

func (s *Store) queryInbox(where string, args ...any) ([]InboxItem, error) {
	rows, err := s.db.Query(`SELECT id, filename, path, size, status, module_id, dest_path, error, created_at FROM inbox `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxItem
	for rows.Next() {
		var it InboxItem
		if err := rows.Scan(&it.ID, &it.Filename, &it.Path, &it.Size, &it.Status, &it.ModuleID, &it.DestPath, &it.Error, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// --- jobs ---

type Job struct {
	ID         string     `json:"id"`
	ModuleID   string     `json:"module_id"`
	Kind       string     `json:"kind"`
	SessionID  string     `json:"session_id"`
	Status     string     `json:"status"`
	Prompt     string     `json:"prompt"`
	Result     *string    `json:"result"`
	Error      *string    `json:"error"`
	CostUSD    *float64   `json:"cost_usd"`
	Denials    *string    `json:"denials"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

func (s *Store) InsertJob(j *Job) error {
	_, err := s.db.Exec(`INSERT INTO jobs (id, module_id, kind, session_id, status, prompt, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.ModuleID, j.Kind, j.SessionID, j.Status, j.Prompt, j.CreatedAt)
	return err
}

func (s *Store) UpdateJob(j *Job) error {
	_, err := s.db.Exec(`UPDATE jobs SET session_id = ?, status = ?, result = ?, error = ?, cost_usd = ?, denials = ?, finished_at = ? WHERE id = ?`,
		j.SessionID, j.Status, j.Result, j.Error, j.CostUSD, j.Denials, j.FinishedAt, j.ID)
	return err
}

func (s *Store) GetJob(id string) (*Job, error) {
	jobs, err := s.queryJobs(`WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, ErrNotFound
	}
	return &jobs[0], nil
}

func (s *Store) ListJobs(moduleID string, limit int) ([]Job, error) {
	if moduleID == "" {
		return s.queryJobs(`ORDER BY created_at DESC LIMIT ?`, limit)
	}
	return s.queryJobs(`WHERE module_id = ? ORDER BY created_at DESC LIMIT ?`, moduleID, limit)
}

// FailRunningJobs marks jobs orphaned by a restart.
func (s *Store) FailRunningJobs() error {
	_, err := s.db.Exec(`UPDATE jobs SET status = 'failed', error = 'server restarted', finished_at = ? WHERE status IN ('queued', 'running')`, time.Now().UTC())
	return err
}

func (s *Store) queryJobs(where string, args ...any) ([]Job, error) {
	rows, err := s.db.Query(`SELECT id, module_id, kind, session_id, status, prompt, result, error, cost_usd, denials, created_at, finished_at FROM jobs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var fin sql.NullTime
		if err := rows.Scan(&j.ID, &j.ModuleID, &j.Kind, &j.SessionID, &j.Status, &j.Prompt, &j.Result, &j.Error, &j.CostUSD, &j.Denials, &j.CreatedAt, &fin); err != nil {
			return nil, err
		}
		if fin.Valid {
			j.FinishedAt = &fin.Time
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
