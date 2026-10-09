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
			Handler(c.ping).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != c.want {
				t.Errorf("/readyz = %d, want %d", rec.Code, c.want)
			}
		})
	}
}
