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
// and run again on request rather than retried.
func insertOpts() river.InsertOpts { return river.InsertOpts{Queue: Queue, MaxAttempts: 1} }

// ImportArgs runs the registered importer Importer.
type ImportArgs struct {
	Importer string `json:"importer"`
}

func (ImportArgs) Kind() string                 { return "import" }
func (ImportArgs) InsertOpts() river.InsertOpts { return insertOpts() }

// GenerateArgs runs the generator.
type GenerateArgs struct{}

func (GenerateArgs) Kind() string                 { return "generate" }
func (GenerateArgs) InsertOpts() river.InsertOpts { return insertOpts() }

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
	imp := w.reg[job.Args.Importer]
	if imp == nil {
		return fmt.Errorf("no importer %q", job.Args.Importer)
	}
	run, err := importers.Execute(ctx, w.s, imp)
	if err != nil {
		return fmt.Errorf("import run %s: %w", run.ID, err)
	}
	return river.RecordOutput(ctx, ImportOutput{Run: run.ID, InputVersion: run.InputVersion, Summary: json.RawMessage(run.Summary)})
}

type generateWorker struct {
	river.WorkerDefaults[GenerateArgs]
	s *store.Store
}

func (w *generateWorker) Work(ctx context.Context, job *river.Job[GenerateArgs]) error {
	run, err := generator.Run(ctx, w.s)
	if err != nil {
		return fmt.Errorf("generator run %s: %w", run.ID, err)
	}
	return river.RecordOutput(ctx, GenerateOutput{Run: run.ID, InputDigest: run.InputDigest, InputImportRuns: run.InputImportRuns,
		Digest: run.Snapshot.Digest(), Report: run.Report})
}

// New returns a client working the catalog queue: import jobs run the
// importers of reg, generate jobs the generator, both on s.
func New(db *sql.DB, s *store.Store, reg importers.Registry) (*river.Client[*sql.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &importWorker{s: s, reg: reg})
	river.AddWorker(workers, &generateWorker{s: s})
	return river.NewClient(riverdatabasesql.New(db), &river.Config{
		Queues:  map[string]river.QueueConfig{Queue: {MaxWorkers: 1}},
		Workers: workers,
	})
}

// Wait polls job id until it finished, since any client may work it, and
// returns the output it recorded or the error of its last attempt.
func Wait(ctx context.Context, c *river.Client[*sql.Tx], id int64) ([]byte, error) {
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
