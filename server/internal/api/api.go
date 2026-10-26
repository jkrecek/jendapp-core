// Package api is the HTTP API consumed by the iOS/macOS app.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"jendapp/internal/claude"
	"jendapp/internal/inbox"
	"jendapp/internal/module"
	"jendapp/internal/store"
)

const maxUpload = 200 << 20

type API struct {
	Store   *store.Store
	Modules *module.Registry
	Runner  *claude.Runner
	Inbox   *inbox.Inbox
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

	mux.HandleFunc("GET /api/modules", a.listModules)
	mux.HandleFunc("GET /api/modules/{id}/sessions", a.listSessions)
	mux.HandleFunc("GET /api/modules/{id}/sessions/{sid}", a.sessionMessages)
	mux.HandleFunc("POST /api/modules/{id}/chat", a.chat)

	mux.HandleFunc("GET /api/jobs", a.listJobs)
	mux.HandleFunc("GET /api/jobs/{id}", a.getJob)
	mux.HandleFunc("GET /api/jobs/{id}/events", a.jobEvents)

	mux.HandleFunc("POST /api/inbox", a.upload)
	mux.HandleFunc("GET /api/inbox", a.listInbox)
	mux.HandleFunc("POST /api/inbox/{id}/assign", a.assign)

	return a.auth(mux)
}

func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeErr(w, http.StatusUnauthorized, "missing token")
			return
		}
		if _, ok := a.Store.AuthDevice(token); !ok {
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type moduleView struct {
	*module.Module
	Available bool `json:"available"`
	CanImport bool `json:"can_import"`
}

func (a *API) listModules(w http.ResponseWriter, r *http.Request) {
	out := []moduleView{}
	for _, m := range a.Modules.All() {
		out = append(out, moduleView{Module: m, Available: m.Exists(), CanImport: len(m.Import) > 0})
	}
	writeJSON(w, 200, out)
}

func (a *API) module(w http.ResponseWriter, r *http.Request) (*module.Module, bool) {
	m, ok := a.Modules.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown module")
	}
	return m, ok
}

func (a *API) listSessions(w http.ResponseWriter, r *http.Request) {
	m, ok := a.module(w, r)
	if !ok {
		return
	}
	s, err := claude.ListSessions(m.Dir)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, s)
}

func (a *API) sessionMessages(w http.ResponseWriter, r *http.Request) {
	m, ok := a.module(w, r)
	if !ok {
		return
	}
	msgs, err := claude.SessionMessages(m.Dir, r.PathValue("sid"))
	if errors.Is(err, os.ErrNotExist) {
		writeErr(w, 404, "unknown session")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, msgs)
}

// chat starts a Claude run and returns the job; stream it via /api/jobs/{id}/events.
func (a *API) chat(w http.ResponseWriter, r *http.Request) {
	m, ok := a.module(w, r)
	if !ok {
		return
	}
	var req struct {
		Message   string `json:"message"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		writeErr(w, 400, "message is required")
		return
	}
	j, err := a.Runner.Start(m, "chat", req.Message, req.SessionID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, j)
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := a.Store.ListJobs(r.URL.Query().Get("module"), limit(r, 50))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	writeJSON(w, 200, jobs)
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := a.Store.GetJob(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "unknown job")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, j)
}

// jobEvents is an SSE stream: each Claude stream-json line as a "data:" event,
// then a final "end" event carrying the job record.
func (a *API) jobEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.Store.GetJob(id); err != nil {
		writeErr(w, 404, "unknown job")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	_, err := a.Runner.Stream(r.Context(), id, func(line []byte) error {
		if _, err := w.Write(append(append([]byte("data: "), line...), '\n', '\n')); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if err != nil {
		return // client gone
	}
	j, err := a.Store.GetJob(id)
	if err != nil {
		return
	}
	b, _ := json.Marshal(j)
	w.Write([]byte("event: end\ndata: "))
	w.Write(b)
	w.Write([]byte("\n\n"))
	flusher.Flush()
}

func (a *API) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, "expected multipart/form-data")
		return
	}
	items := []*store.InboxItem{}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		it, err := a.Inbox.Add(part.FileName(), part)
		part.Close()
		if err != nil {
			slog.Error("upload", "file", part.FileName(), "err", err)
			writeErr(w, 400, err.Error())
			return
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		writeErr(w, 400, "no files in field \"file\"")
		return
	}
	writeJSON(w, http.StatusCreated, items)
}

func (a *API) listInbox(w http.ResponseWriter, r *http.Request) {
	items, err := a.Store.ListInbox(r.URL.Query().Get("status"), limit(r, 100))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if items == nil {
		items = []store.InboxItem{}
	}
	writeJSON(w, 200, items)
}

func (a *API) assign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ModuleID string `json:"module_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ModuleID == "" {
		writeErr(w, 400, "module_id is required")
		return
	}
	it, err := a.Inbox.Assign(r.PathValue("id"), req.ModuleID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, "unknown inbox item")
	case err != nil:
		writeErr(w, 400, err.Error())
	default:
		writeJSON(w, 200, it)
	}
}

func limit(r *http.Request, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		return n
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
