// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package jobs runs import and generator runs and model imports as River jobs on the catalog queue,
// and delivers webhooks on the webhooks queue.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/fishman/neocloud-catalog/service/internal/broker"
	"github.com/fishman/neocloud-catalog/service/internal/generator"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Queue is the catalog's job queue.
const Queue = "catalog"

// Migrate applies River's own schema; idempotent.
func Migrate(ctx context.Context, db *sql.DB) error {
	m, err := rivermigrate.New(riverdatabasesql.New(db), nil)
	if err != nil {
		return err
	}
	_, err = m.Migrate(ctx, rivermigrate.DirectionUp, nil)
	return err
}

// insertOpts allows one attempt: a retry would leave the first attempt's run
// row behind, so a failed run is run again on request instead.
func insertOpts() river.InsertOpts { return river.InsertOpts{Queue: Queue, MaxAttempts: 1} }

// ImportArgs runs Importer as import run Run, so the enqueuer knows the run up front.
type ImportArgs struct {
	Importer string    `json:"importer"`
	Run      uuid.UUID `json:"run"`
}

func (ImportArgs) Kind() string                 { return "import" }
func (ImportArgs) InsertOpts() river.InsertOpts { return insertOpts() }

// GenerateArgs runs the generator as generator run Run.
type GenerateArgs struct {
	Run uuid.UUID `json:"run"`
}

func (GenerateArgs) Kind() string                 { return "generate" }
func (GenerateArgs) InsertOpts() river.InsertOpts { return insertOpts() }

// FailAbandonedArgs fails runs left running by exited workers.
type FailAbandonedArgs struct{}

func (FailAbandonedArgs) Kind() string                 { return "fail_abandoned_runs" }
func (FailAbandonedArgs) InsertOpts() river.InsertOpts { return insertOpts() }

// ModelImportArgs runs the queued model import Import into the local layer.
type ModelImportArgs struct {
	Import uuid.UUID `json:"import"`
}

func (ModelImportArgs) Kind() string                 { return "model_import" }
func (ModelImportArgs) InsertOpts() river.InsertOpts { return insertOpts() }

// ImportOutput is what a completed import job records.
type ImportOutput struct {
	Run          uuid.UUID       `json:"run"`
	InputVersion string          `json:"input_version"`
	Summary      json.RawMessage `json:"summary"`
}

// GenerateOutput is what a completed generate job records.
type GenerateOutput struct {
	Run             uuid.UUID   `json:"run"`
	InputDigest     string      `json:"input_digest"`
	InputImportRuns []uuid.UUID `json:"input_import_runs"`
	Digest          string      `json:"digest"`
	Report          []string    `json:"report"`
}

type importWorker struct {
	river.WorkerDefaults[ImportArgs]
	s   *store.Store
	reg importers.Registry
}

func (w *importWorker) Work(ctx context.Context, job *river.Job[ImportArgs]) error {
	imp, err := w.reg.Lookup(job.Args.Importer)
	if err != nil {
		return err
	}
	run, err := importers.Execute(ctx, w.s, imp, job.Args.Run)
	if err != nil {
		return err
	}
	return river.RecordOutput(ctx, ImportOutput{Run: run.ID, InputVersion: run.InputVersion, Summary: json.RawMessage(run.Summary)})
}

type generateWorker struct {
	river.WorkerDefaults[GenerateArgs]
	s *store.Store
}

func (w *generateWorker) Work(ctx context.Context, job *river.Job[GenerateArgs]) error {
	run, err := generator.Run(ctx, w.s, job.Args.Run)
	if err != nil {
		return err
	}
	return river.RecordOutput(ctx, GenerateOutput{Run: run.ID, InputDigest: run.InputDigest, InputImportRuns: run.InputImportRuns,
		Digest: run.Snapshot.Digest(), Report: run.Report})
}

type failAbandonedWorker struct {
	river.WorkerDefaults[FailAbandonedArgs]
	s         *store.Store
	olderThan time.Duration
}

func (w *failAbandonedWorker) Work(ctx context.Context, _ *river.Job[FailAbandonedArgs]) error {
	_, err := w.s.FailAbandonedRuns(ctx, w.olderThan)
	return err
}

type modelImportWorker struct {
	river.WorkerDefaults[ModelImportArgs]
	b *broker.Broker
}

func (w *modelImportWorker) Work(ctx context.Context, job *river.Job[ModelImportArgs]) error {
	if w.b == nil {
		return errors.New("model imports are not configured")
	}
	return w.b.Run(ctx, job.Args.Import)
}

// Config: JobTimeout cancels a job (its run recorded failed); after RescueAfter
// (> JobTimeout) River rescues a job left by an exited worker and its run is failed.
// Broker runs model imports; Webhooks are notified of published releases, retried
// after WebhookBackoff doubling per attempt (default 30s).
type Config struct {
	JobTimeout, RescueAfter time.Duration
	Broker                  *broker.Broker
	Webhooks                []Subscriber
	WebhookBackoff          time.Duration
}

// Client works the catalog and webhooks queues.
type Client struct {
	*river.Client[*sql.Tx]
	s           *store.Store
	rescueAfter time.Duration
	webhooks    []Subscriber
}

// New returns a client running import, generate and model import jobs and
// webhook deliveries, plus a periodic sweep of abandoned runs.
func New(db *sql.DB, s *store.Store, reg importers.Registry, cfg Config) (*Client, error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &importWorker{s: s, reg: reg})
	river.AddWorker(workers, &generateWorker{s: s})
	river.AddWorker(workers, &modelImportWorker{b: cfg.Broker})
	river.AddWorker(workers, newWebhookWorker(cfg.Webhooks, cfg.WebhookBackoff))
	river.AddWorker(workers, &failAbandonedWorker{s: s, olderThan: cfg.RescueAfter})
	c, err := river.NewClient(riverdatabasesql.New(db), &river.Config{
		Queues:               map[string]river.QueueConfig{Queue: {MaxWorkers: 1}, WebhookQueue: {MaxWorkers: webhookWorkers}},
		Workers:              workers,
		JobTimeout:           cfg.JobTimeout,
		RescueStuckJobsAfter: cfg.RescueAfter,
		PeriodicJobs: []*river.PeriodicJob{river.NewPeriodicJob(river.PeriodicInterval(cfg.RescueAfter/2),
			func() (river.JobArgs, *river.InsertOpts) { return FailAbandonedArgs{}, nil }, nil)},
	})
	if err != nil {
		return nil, err
	}
	return &Client{Client: c, s: s, rescueAfter: cfg.RescueAfter, webhooks: cfg.Webhooks}, nil
}

// Start fails abandoned runs, then works the queue.
func (c *Client) Start(ctx context.Context) error {
	if _, err := c.s.FailAbandonedRuns(ctx, c.rescueAfter); err != nil {
		return fmt.Errorf("fail abandoned runs: %w", err)
	}
	return c.Client.Start(ctx)
}

// Shutdown lets running jobs finish within grace, then cancels them; a
// cancelled run is recorded failed.
func (c *Client) Shutdown(grace time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := c.Stop(ctx); ctx.Err() == nil {
		return err
	}
	return c.StopAndCancel(context.Background())
}

// Job returns the job of kind ("import" or "generate") running run, or nil.
func (c *Client) Job(ctx context.Context, kind string, run uuid.UUID) (*rivertype.JobRow, error) {
	res, err := c.JobList(ctx, river.NewJobListParams().Kinds(kind).
		Where("args @> @run::jsonb", river.NamedArgs{"run": `{"run":"` + run.String() + `"}`}).First(1))
	if err != nil || len(res.Jobs) == 0 {
		return nil, err
	}
	return res.Jobs[0], nil
}

// Queued returns the jobs of kind no worker has started yet, newest first.
func (c *Client) Queued(ctx context.Context, kind string) ([]*rivertype.JobRow, error) {
	res, err := c.JobList(ctx, river.NewJobListParams().Kinds(kind).
		States(rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable, rivertype.JobStateScheduled).
		OrderBy(river.JobListOrderByID, river.SortOrderDesc).First(1000))
	if err != nil {
		return nil, err
	}
	return res.Jobs, nil
}

// Wait polls job id until it finishes (any client may work it) and returns its
// recorded output or its last attempt's error.
func Wait(ctx context.Context, c *Client, id int64) ([]byte, error) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		job, err := c.JobGet(ctx, id)
		if err != nil {
			return nil, err
		}
		switch job.State {
		case rivertype.JobStateCompleted:
			return job.Output(), nil
		case rivertype.JobStateDiscarded, rivertype.JobStateCancelled:
			if n := len(job.Errors); n > 0 {
				return nil, errors.New(job.Errors[n-1].Error)
			}
			return nil, fmt.Errorf("job %d %s", id, job.State)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}
