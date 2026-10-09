package huggingface

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxBody bounds a response the importer reads; a tree of a large repository
// stays well below it.
const maxBody = 64 << 20

// client reads the Hub for one run, spacing its requests.
type client struct {
	*Importer
	last time.Time
}

// httpError is a response the Hub refused a request with.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.msg) }

// get reads rawURL. It spaces requests by interval and retries transport
// errors, 429 and 5xx up to retries times, after Retry-After when the
// response has one, else after a doubling backoff.
func (c *client) get(ctx context.Context, rawURL string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for attempt := 0; ; attempt++ {
		if err := sleep(ctx, time.Until(c.last.Add(c.interval))); err != nil {
			return nil, nil, err
		}
		c.last = time.Now()
		wait, retry := c.backoff<<attempt, true
		resp, err := c.client.Do(req)
		if err == nil {
			var body []byte
			body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
			resp.Body.Close()
			switch code := resp.StatusCode; {
			case err != nil:
			case code == http.StatusOK:
				return body, resp.Header, nil
			case code == http.StatusUnauthorized || code == http.StatusForbidden:
				err, retry = c.gated(&httpError{code, message(body)}), false
			default:
				err = &httpError{code, message(body)}
				retry = code == http.StatusTooManyRequests || code >= 500
				if d, ok := retryAfter(resp.Header.Get("Retry-After")); ok {
					wait = d
				}
			}
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if !retry || attempt == c.retries {
			return nil, nil, fmt.Errorf("GET %s: %w", req.URL.Path, err)
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, nil, err
		}
	}
}

// getJSON decodes the response of rawURL into v.
func (c *client) getJSON(ctx context.Context, rawURL string, v any) error {
	body, _, err := c.get(ctx, rawURL)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		u, _ := url.Parse(rawURL)
		return fmt.Errorf("GET %s: %w", u.Path, err)
	}
	return nil
}

// tree reads a file tree page by page, following each page's Link rel="next"
// on the Hub's host only, so the token goes nowhere else.
func (c *client) tree(ctx context.Context, rawURL string) ([]entry, error) {
	host := ""
	if u, err := url.Parse(c.endpoint); err == nil {
		host = u.Host
	}
	var all []entry
	for rawURL != "" {
		body, h, err := c.get(ctx, rawURL)
		if err != nil {
			return nil, err
		}
		var page []entry
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("tree: %w", err)
		}
		all = append(all, page...)
		rawURL = nextLink(h.Get("Link"))
		if u, err := url.Parse(rawURL); rawURL != "" && (err != nil || u.Host != host) {
			return nil, fmt.Errorf("tree: next page %q is not on %s", rawURL, host)
		}
	}
	return all, nil
}

// gated names why the Hub refused a gated repository.
func (c *client) gated(err error) error {
	if c.token == "" {
		return fmt.Errorf("gated: token missing (huggingface.token_file): %w", err)
	}
	return fmt.Errorf("gated: token rejected or licence not accepted: %w", err)
}

// message is the Hub's error message in body: {"error": ...} or plain text.
func message(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
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

// nextLink is the URL of the rel="next" entry of a Link header, or "".
func nextLink(h string) string {
	for part := range strings.SplitSeq(h, ",") {
		target, params, ok := strings.Cut(part, ";")
		if ok && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}

func sleep(ctx context.Context, d time.Duration) error {
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
