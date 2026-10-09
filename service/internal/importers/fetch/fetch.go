// Package fetch reads HTTP responses for importers: bounded bodies, and
// retries of transport errors, 429 and 5xx after the response's Retry-After
// (bounded) or a doubling backoff.
package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Getter reads responses for one importer. Retries bounds the retries of a
// request, Backoff is the first wait between attempts, MaxBody the bytes of a
// body and MaxRetryAfter the longest Retry-After waited out: an importer
// asked to wait longer fails the request rather than spend jobs.job_timeout.
type Getter struct {
	Client        *http.Client
	Backoff       time.Duration
	Retries       int
	MaxBody       int64
	MaxRetryAfter time.Duration
}

// StatusError is a response refused with a status other than 200; Body holds
// at most MaxBody bytes of it. Wait is the Retry-After beyond MaxRetryAfter
// the request gave up on, else 0.
type StatusError struct {
	Code      int
	Status    string
	Body      []byte
	Wait, Max time.Duration
}

func (e *StatusError) Error() string {
	if e.Wait > 0 {
		return fmt.Sprintf("HTTP %s (Retry-After %s exceeds %s)", e.Status, e.Wait, e.Max)
	}
	return "HTTP " + e.Status
}

// Get sends req, a request without a body, until it gets a 200 and returns
// that body and header. It retries transport errors, 429 and 5xx up to
// Retries times, after Retry-After (at most MaxRetryAfter) when the response
// has one, else after Backoff doubled per attempt. A body over MaxBody fails
// the request. A cancelled ctx returns ctx.Err() as it is.
func (g Getter) Get(ctx context.Context, req *http.Request) ([]byte, http.Header, error) {
	for attempt := 0; ; attempt++ {
		wait, retry := g.Backoff<<attempt, true
		resp, err := g.Client.Do(req.WithContext(ctx))
		if err == nil {
			var body []byte
			body, err = io.ReadAll(io.LimitReader(resp.Body, g.MaxBody+1))
			resp.Body.Close()
			switch code := resp.StatusCode; {
			case err != nil:
			case code == http.StatusOK && int64(len(body)) > g.MaxBody:
				err, retry = fmt.Errorf("body exceeds %d bytes", g.MaxBody), false
			case code == http.StatusOK:
				return body, resp.Header, nil
			default:
				se := &StatusError{Code: code, Status: resp.Status, Body: body[:min(int64(len(body)), g.MaxBody)]}
				err, retry = se, code == http.StatusTooManyRequests || code >= 500
				if d, ok := retryAfter(resp.Header.Get("Retry-After")); ok {
					wait = d
				}
				if retry && wait > g.MaxRetryAfter {
					se.Wait, se.Max, retry = wait, g.MaxRetryAfter, false
				}
			}
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if !retry || attempt == g.Retries {
			return nil, nil, err
		}
		if err := Sleep(ctx, wait); err != nil {
			return nil, nil, err
		}
	}
}

// retryAfter reads a Retry-After of seconds or an HTTP date.
func retryAfter(v string) (time.Duration, bool) {
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

// Sleep waits d, or returns ctx.Err() once ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
