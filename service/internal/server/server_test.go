// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadyzFollowsDatabasePing(t *testing.T) {
	cases := map[string]struct {
		ping func(context.Context) error
		want int
	}{
		"database answers": {func(context.Context) error { return nil }, http.StatusNoContent},
		"database down":    {func(context.Context) error { return errors.New("connection refused") }, http.StatusServiceUnavailable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Handler(c.ping, http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != c.want {
				t.Errorf("/readyz = %d, want %d", rec.Code, c.want)
			}
		})
	}
}

// /v1 is the API's; the probes stay outside it.
func TestHandlerServesTheAPIUnderV1(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := Handler(func(context.Context) error { return nil }, api)
	for path, want := range map[string]int{"/v1/catalog": http.StatusTeapot, "/v1/local/node-types/azure%2Fx": http.StatusTeapot,
		"/healthz": http.StatusNoContent, "/catalog": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}
