// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An oversized body fails without retry; a refusal is a StatusError with code
// and body.
func TestGetBoundsBodiesAndTypesRefusals(t *testing.T) {
	var calls atomic.Int32
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(code)
		w.Write([]byte("0123456789"))
	}))
	t.Cleanup(srv.Close)
	g := Getter{Client: srv.Client(), Backoff: time.Millisecond, Retries: 3, MaxBody: 10, MaxRetryAfter: time.Minute}
	get := func() ([]byte, error) {
		calls.Store(0)
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _, err := g.Get(context.Background(), req)
		return b, err
	}
	if b, err := get(); err != nil || string(b) != "0123456789" {
		t.Errorf("body of MaxBody bytes = %q, %v", b, err)
	}
	g.MaxBody = 9
	if b, err := get(); err == nil || !strings.Contains(err.Error(), "body exceeds 9 bytes") || b != nil || calls.Load() != 1 {
		t.Errorf("over-cap body = %q, %v after %d calls, want one failed call", b, err, calls.Load())
	}
	code = http.StatusNotFound
	var se *StatusError
	if _, err := get(); !errors.As(err, &se) || se.Code != http.StatusNotFound || string(se.Body) != "012345678" || err.Error() != "HTTP 404 Not Found" ||
		calls.Load() != 1 {
		t.Errorf("404 = %v (%+v) after %d calls, want a StatusError with the capped body, not retried", err, se, calls.Load())
	}
}
