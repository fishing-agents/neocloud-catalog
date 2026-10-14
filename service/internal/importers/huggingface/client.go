// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package huggingface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fishman/neocloud-catalog/service/internal/importers/fetch"
)

const maxBody = 64 << 20

// A longer Retry-After fails the request so one source cannot spend
// jobs.job_timeout.
const maxRetryAfter = 5 * time.Minute

type client struct {
	*Importer
	last time.Time
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.msg) }

// get treats 401 and 403 as gated refusals.
func (c *client) get(ctx context.Context, rawURL string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if err := fetch.Sleep(ctx, time.Until(c.last.Add(c.interval))); err != nil {
		return nil, nil, err
	}
	c.last = time.Now()
	body, h, err := c.getter.Get(ctx, req)
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if se := (*fetch.StatusError)(nil); errors.As(err, &se) {
		herr := &httpError{se.Code, message(se.Body)}
		switch {
		case se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden:
			err = c.gated(herr)
		case se.Wait > 0:
			err = fmt.Errorf("%w (Retry-After %s exceeds %s)", herr, se.Wait, se.Max)
		default:
			err = herr
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: %w", req.URL.Path, err)
	}
	return body, h, nil
}

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

// tree follows Link rel="next" on the Hub's host only, so the token goes
// nowhere else.
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

func (c *client) gated(err error) error {
	if c.token == "" {
		return fmt.Errorf("gated: token missing (huggingface.token_file): %w", err)
	}
	return fmt.Errorf("gated: token rejected or licence not accepted: %w", err)
}

// message is the Hub's error: {"error": ...} or plain text.
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

func nextLink(h string) string {
	for part := range strings.SplitSeq(h, ",") {
		target, params, ok := strings.Cut(part, ";")
		if ok && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}
