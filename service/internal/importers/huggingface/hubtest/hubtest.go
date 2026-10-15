// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package hubtest serves recorded Hugging Face Hub answers to tests: testdata/hf/<repo>/<revision>/
// <call>.<status>.<ext> for model info, recursive trees and config.json, the default branch "main" as
// a repository's recorded commit, private repositories behind a token, and searches.
package hubtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Private serves the recorded repository Recorded under another id, only with Token.
type Private struct {
	Recorded, Token string
}

// Hub is the stub Hub; its counters and seen Authorization headers are safe to read concurrently.
type Hub struct {
	*httptest.Server
	mu       sync.Mutex
	searches int
	auth     []string
}

// SearchHits are what every search answers.
var SearchHits = []map[string]any{
	{"id": "Qwen/Qwen2.5-14B-Instruct", "pipeline_tag": "text-generation", "library_name": "transformers", "downloads": 1346684, "likes": 371},
	{"id": "bartowski/Qwen2.5-14B-Instruct-GGUF", "pipeline_tag": "text-generation", "downloads": 52000, "likes": 40},
}

// New starts the Hub with private repositories by id.
func New(t testing.TB, private map[string]Private) *Hub {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(self), "../../../../testdata/hf")
	h := &Hub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.auth = append(h.auth, r.Header.Get("Authorization"))
		h.mu.Unlock()
		if r.URL.Path == "/api/models" {
			h.mu.Lock()
			h.searches++
			h.mu.Unlock()
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			writeJSON(w, http.StatusOK, SearchHits[:min(limit, len(SearchHits))])
			return
		}
		repo, rev, call, ok := route(r.URL.Path)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
			return
		}
		isPrivate := false
		if p, ok := private[repo]; ok {
			if r.Header.Get("Authorization") != "Bearer "+p.Token {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Repository not found"})
				return
			}
			repo, isPrivate = p.Recorded, true
		}
		if rev == "main" {
			commits, _ := filepath.Glob(filepath.Join(dir, repo, strings.Repeat("?", 40), "info.200.json"))
			if len(commits) == 1 {
				rev = filepath.Base(filepath.Dir(commits[0]))
			}
		}
		matches, _ := filepath.Glob(filepath.Join(dir, repo, rev, call+".*"))
		if len(matches) != 1 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Entry not found"})
			return
		}
		name := strings.Split(filepath.Base(matches[0]), ".")
		status, _ := strconv.Atoi(name[1])
		body, err := os.ReadFile(matches[0])
		if err != nil {
			t.Errorf("fixture %s: %v", matches[0], err)
		}
		if isPrivate && call == "info" {
			var info map[string]any
			if err := json.Unmarshal(body, &info); err != nil {
				t.Errorf("fixture %s: %v", matches[0], err)
			}
			info["private"] = true
			body, _ = json.Marshal(info)
		}
		w.Header().Set("Content-Type", map[string]string{"json": "application/json", "txt": "text/plain"}[name[2]])
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(h.Close)
	return h
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// route splits /api/models/<repo>/revision/<rev>, /api/models/<repo>/tree/<rev> and
// /<repo>/resolve/<rev>/config.json.
func route(p string) (repo, rev, call string, ok bool) {
	s := strings.Split(strings.TrimPrefix(p, "/"), "/")
	switch {
	case len(s) == 6 && s[0] == "api" && s[1] == "models" && s[4] == "revision":
		return s[2] + "/" + s[3], s[5], "info", true
	case len(s) == 6 && s[0] == "api" && s[1] == "models" && s[4] == "tree":
		return s[2] + "/" + s[3], s[5], "tree", true
	case len(s) == 5 && s[2] == "resolve" && s[4] == "config.json":
		return s[0] + "/" + s[1], s[3], "config", true
	}
	return "", "", "", false
}

// Searches counts the searches answered.
func (h *Hub) Searches() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.searches
}

// Authorizations are the Authorization headers of every request, in order.
func (h *Hub) Authorizations() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.auth...)
}
