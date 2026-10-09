// Package httpapi serves the catalog service's /v1 API. api/openapi.yaml is
// the contract: gen.gen.go is generated from it (make generate), the handlers
// implement its ServerInterface.
package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/fishman/neocloud-catalog/service/internal/auth"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/release"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Config is what the API serves.
type Config struct {
	Store     *store.Store
	Auth      auth.Authenticator
	Jobs      *jobs.Client
	Importers importers.Registry
	// Publish is where releases are published; nil refuses publishing.
	Publish *release.Target
}

type handlers struct{ Config }

// New returns the API: every request needs a valid bearer token, and writes a
// principal that may write.
func New(c Config) http.Handler {
	api := HandlerWithOptions(&handlers{c}, StdHTTPServerOptions{
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		},
	})
	return authenticate(c.Auth, api)
}

// authenticate answers 401 without a valid bearer token and 403 to a write
// (any method but GET and HEAD) by a principal that may only read. It never
// logs the token.
func authenticate(a auth.Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || token == "" {
			unauthorized(w)
			return
		}
		p, err := a.Authenticate(r.Context(), token)
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthorized) {
				log.Printf("authenticate: %v", err)
			}
			unauthorized(w)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !p.CanWrite() {
			writeError(w, http.StatusForbidden, "forbidden", "writes need an API key or the admin role")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, Error{Code: code, Message: msg})
}

// fail answers err: a missing object 404, refused objects 400, a refused
// change 409, anything else 500 with the error logged, not returned.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

// decode reads the JSON body into v, or answers 400 and returns false.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body: "+err.Error())
		return false
	}
	return true
}

// orEmpty keeps an empty list a list, not null.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
