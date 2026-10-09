// Package jobs runs import and generator runs as River jobs on the catalog
// queue.
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

	"github.com/fishman/neocloud-catalog/service/internal/generator"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Queue is the queue of the catalog's jobs.
const Queue = "catalog"

// Migrate applies River's own schema; its migrator is idempotent and
// versioned.
func Migrate(ctx context.Context, db *sql.DB) error {
	m, err := rivermigrate.New(riverdatabasesql.New(db), nil)
	if err != nil {
		return err
	}
	_, err = m.Migrate(ctx, rivermigrate.DirectionUp, nil)
	return err
}

// insertOpts makes a job one attempt: its run is recorded, a failed one too,
// and run again on request rather than retried; a retry would leave the
// first attempt's run row behind.
func insertOpts() river.InsertOpts { return river.InsertOpts{Queue: Queue, MaxAttempts: 1} }

// ImportArgs runs the registered importer Importer as the import run Run, so
// whoever enqueues it knows the run before it starts.
type ImportArgs struct {
	Importer string    `json:"importer"`
	Run      uuid.UUID `json:"run"`
}

func (ImportArgs) Kind() string                 { return "import" }
func (ImportArgs) InsertOpts() river.InsertOpts { return insertOpts() }

// GenerateArgs runs the generator as the generator run Run.
type GenerateArgs struct {
	Run uuid.UUID `json:"run"`
}

func (GenerateArgs) Kind() string                 { return "generate" }
func (GenerateArgs) InsertOpts() river.InsertOpts { return insertOpts() }

// FailAbandonedArgs fails the runs workers that exited left running.
type FailAbandonedArgs struct{}

func (FailAbandonedArgs) Kind() string                 { return "fail_abandoned_runs" }
func (FailAbandonedArgs) InsertOpts() river.InsertOpts { return insertOpts() }

// ImportOutput is what a completed import job records.
type ImportOutput struct {
	Run          uuid.UUID       `json:"run"`
	InputVersion string          `json:"input_version"`
	Summary      json.RawMessage `json:"summary"`
}

// GenerateOutput is what a completed generate job records; Digest is the
// stored snapshot's, whose version a release sets.
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

// Config bounds the runs: JobTimeout cancels a job that runs longer (the run
// is recorded as failed); RescueAfter (> JobTimeout) is when River rescues a
// job left running by a worker that exited, and when its run, still running,
// is failed.
type Config struct {
	JobTimeout, RescueAfter time.Duration
}

// Client works the catalog queue.
type Client struct {
	*river.Client[*sql.Tx]
	s           *store.Store
	rescueAfter time.Duration
}

// New returns a client working the catalog queue: import jobs run the
// importers of reg, generate jobs the generator, both on s, and a periodic
// job fails abandoned runs.
func New(db *sql.DB, s *store.Store, reg importers.Registry, cfg Config) (*Client, error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &importWorker{s: s, reg: reg})
	river.AddWorker(workers, &generateWorker{s: s})
	river.AddWorker(workers, &failAbandonedWorker{s: s, olderThan: cfg.RescueAfter})
	c, err := river.NewClient(riverdatabasesql.New(db), &river.Config{
		Queues:               map[string]river.QueueConfig{Queue: {MaxWorkers: 1}},
		Workers:              workers,
		JobTimeout:           cfg.JobTimeout,
		RescueStuckJobsAfter: cfg.RescueAfter,
		PeriodicJobs: []*river.PeriodicJob{river.NewPeriodicJob(river.PeriodicInterval(cfg.RescueAfter/2),
			func() (river.JobArgs, *river.InsertOpts) { return FailAbandonedArgs{}, nil }, nil)},
	})
	if err != nil {
		return nil, err
	}
	return &Client{Client: c, s: s, rescueAfter: cfg.RescueAfter}, nil
}

// Start fails the runs workers that exited left running, then works the
// queue.
func (c *Client) Start(ctx context.Context) error {
	if _, err := c.s.FailAbandonedRuns(ctx, c.rescueAfter); err != nil {
		return fmt.Errorf("fail abandoned runs: %w", err)
	}
	return c.Client.Start(ctx)
}

// Shutdown stops working the queue: it lets running jobs finish within
// grace, then cancels them, so a long import does not hold the process; a
// cancelled run is recorded as failed.
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

// Wait polls job id until it finished, since any client may work it, and
// returns the output it recorded or the error of its last attempt.
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
