package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"jendapp/internal/module"
	"jendapp/internal/store"
)

// viewData runs the view's source with the query params (e.g. ?month=2026-10).
func (a *API) viewData(w http.ResponseWriter, r *http.Request) {
	m, ok := a.module(w, r)
	if !ok {
		return
	}
	v := m.View(r.PathValue("view"))
	if v == nil {
		writeErr(w, 404, "unknown view")
		return
	}
	params := map[string]string{}
	for _, p := range v.Params {
		params[p] = r.URL.Query().Get(p)
	}
	if err := module.ValidateParams(v.Params, params); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	snap, err := a.Ops.Fetch(r.Context(), m, m.Source(v.Source), params)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, snap)
}

type opRequest struct {
	View   string            `json:"view"`
	Params map[string]string `json:"params"`
	RowID  string            `json:"row_id"`
	Inputs map[string]string `json:"inputs"`
}

// runOperation runs a command operation and answers once it has finished (it
// keeps running if the client goes away), or starts a Claude operation and
// returns its job at once; stream that via /api/jobs/{id}/events.
func (a *API) runOperation(w http.ResponseWriter, r *http.Request) {
	m, ok := a.module(w, r)
	if !ok {
		return
	}
	op := m.Operation(r.PathValue("op"))
	if op == nil {
		writeErr(w, 404, "unknown operation")
		return
	}
	var req opRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, "invalid JSON body")
		return
	}
	if err := module.ValidateParams(op.Params, req.Params); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	vars := module.Vars{Params: req.Params, Inputs: map[string]string{}}
	for _, in := range op.Inputs {
		val := strings.TrimSpace(req.Inputs[in.Key])
		if val == "" {
			writeErr(w, 400, in.Title+" is required")
			return
		}
		if in.Type == "text" {
			val = strings.Join(strings.Fields(val), " ") // one line
		}
		vars.Inputs[in.Key] = val
	}
	if op.Scope == "row" {
		row, code, msg := a.findRow(r, m, op, req)
		if row == nil {
			writeErr(w, code, msg)
			return
		}
		vars.Row = row
	}

	if op.Kind == "claude" {
		j, err := a.Runner.Start(m, "op", module.Expand(op.Prompt, vars), "")
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, j)
		return
	}
	j, done, err := a.Ops.Start(m, op, module.ExpandArgs(op.Run, vars))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	select {
	case <-done:
	case <-r.Context().Done():
		return
	}
	if fresh, err := a.Store.GetJob(j.ID); err == nil {
		j = fresh
	}
	writeJSON(w, 200, j)
}

// findRow resolves a row operation's row from the view's last snapshot, so row
// values come from the module's own output, never from the client.
func (a *API) findRow(r *http.Request, m *module.Module, op *module.Operation, req opRequest) (map[string]any, int, string) {
	v := m.View(req.View)
	if v == nil || !slices.Contains(v.RowActions, op.ID) {
		return nil, 400, "row operations need the view they were started from"
	}
	if req.RowID == "" {
		return nil, 400, "row_id is required"
	}
	src := m.Source(v.Source)
	snap, err := a.Ops.Cached(m, src, req.Params)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, 500, err.Error()
	}
	var row map[string]any
	if snap != nil {
		row = snap.Row(req.RowID)
	}
	if row == nil { // not loaded yet or new since: refresh once
		if snap, err = a.Ops.Fetch(r.Context(), m, src, req.Params); err == nil && !snap.Stale {
			row = snap.Row(req.RowID)
		}
	}
	if row == nil {
		return nil, 404, "row not found; refresh the view"
	}
	for field, allowed := range op.When {
		if !slices.Contains(allowed, module.Stringify(row[field])) {
			return nil, 409, op.Title + " isn't available for this row"
		}
	}
	return row, 0, ""
}
