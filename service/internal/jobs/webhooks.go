// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

const (
	// WebhookQueue delivers webhooks apart from the catalog queue's long runs.
	WebhookQueue = "webhooks"
	// EventReleasePublished is the event of a published release.
	EventReleasePublished = "release.published"
	// SignatureHeader carries "sha256=" and the hex HMAC-SHA256 of the body with the subscriber's secret.
	SignatureHeader = "X-Neocloud-Signature"

	webhookWorkers  = 4
	webhookAttempts = 12
	webhookTimeout  = 30 * time.Second
	maxWebhookWait  = time.Hour
)

// Subscriber receives webhooks signed with Secret.
type Subscriber struct {
	URL, Secret string
}

// ReleasePublished is a release.published webhook body: Ref the published repository:tag, Digest its
// artifact's manifest digest, Version the release's.
type ReleasePublished struct {
	Event   string `json:"event"`
	Ref     string `json:"ref"`
	Digest  string `json:"digest"`
	Version string `json:"version"`
}

// WebhookArgs delivers Body to URL; the secret stays in the configuration, never in the job. Delivery
// is the same for every attempt.
type WebhookArgs struct {
	URL      string          `json:"url"`
	Event    string          `json:"event"`
	Delivery uuid.UUID       `json:"delivery"`
	Body     json.RawMessage `json:"body"`
}

func (WebhookArgs) Kind() string { return "webhook" }
func (WebhookArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: WebhookQueue, MaxAttempts: webhookAttempts}
}

// Sign is the SignatureHeader value of body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// NotifyPublished queues a release.published webhook to every subscriber.
func (c *Client) NotifyPublished(ctx context.Context, ref, digest, version string) error {
	if len(c.webhooks) == 0 {
		return nil
	}
	body, err := json.Marshal(ReleasePublished{Event: EventReleasePublished, Ref: ref, Digest: digest, Version: version})
	if err != nil {
		return err
	}
	params := make([]river.InsertManyParams, len(c.webhooks))
	for i, sub := range c.webhooks {
		params[i] = river.InsertManyParams{Args: WebhookArgs{URL: sub.URL, Event: EventReleasePublished, Delivery: uuid.New(), Body: body}}
	}
	_, err = c.InsertMany(ctx, params)
	return err
}

type webhookWorker struct {
	river.WorkerDefaults[WebhookArgs]
	secrets map[string]string
	backoff time.Duration
	client  *http.Client
}

func newWebhookWorker(subs []Subscriber, backoff time.Duration) *webhookWorker {
	w := &webhookWorker{secrets: map[string]string{}, backoff: backoff, client: &http.Client{Timeout: webhookTimeout}}
	if w.backoff <= 0 {
		w.backoff = 30 * time.Second
	}
	for _, s := range subs {
		w.secrets[s.URL] = s.Secret
	}
	return w
}

// Work posts the body signed; anything but 2xx is retried, a subscriber no longer configured is not.
func (w *webhookWorker) Work(ctx context.Context, job *river.Job[WebhookArgs]) error {
	secret, ok := w.secrets[job.Args.URL]
	if !ok {
		return river.JobCancel(fmt.Errorf("webhook subscriber %s is no longer configured", job.Args.URL))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.Args.URL, bytes.NewReader(job.Args.Body))
	if err != nil {
		return river.JobCancel(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "neocloud-catalog-service")
	req.Header.Set("X-Neocloud-Event", job.Args.Event)
	req.Header.Set("X-Neocloud-Delivery", job.Args.Delivery.String())
	req.Header.Set(SignatureHeader, Sign(secret, job.Args.Body))
	res, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("webhook %s answered %s", job.Args.URL, res.Status)
	}
	return nil
}

// NextRetry doubles the backoff per attempt, up to an hour.
func (w *webhookWorker) NextRetry(job *river.Job[WebhookArgs]) time.Time {
	return time.Now().Add(min(w.backoff<<min(job.Attempt-1, 20), maxWebhookWait))
}

func (w *webhookWorker) Timeout(*river.Job[WebhookArgs]) time.Duration { return webhookTimeout }
