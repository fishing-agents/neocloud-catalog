package jobs_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Import and generate jobs run as the named runs and record them; a failing job reports its error.
func TestImportAndGenerateJobs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sqlDB, db := dbtest.Fresh(t, "jobs")
	dbtest.Migrate(t, sqlDB)
	if err := jobs.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	imp, err := bootstrap.New("../../../data")
	if err != nil {
		t.Fatal(err)
	}
	c, err := jobs.New(sqlDB, store.New(db), importers.NewRegistry(imp), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Stop(context.Background()) })

	run := func(args river.JobArgs, out any) error {
		t.Helper()
		res, err := c.Insert(ctx, args, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Job.Queue != jobs.Queue {
			t.Errorf("%s job on queue %q, want %q", args.Kind(), res.Job.Queue, jobs.Queue)
		}
		b, err := jobs.Wait(ctx, c, res.Job.ID)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s output %s: %v", args.Kind(), b, err)
		}
		return nil
	}

	var imported jobs.ImportOutput
	importID := uuid.New()
	if err := run(jobs.ImportArgs{Importer: "bootstrap-from-json", Run: importID}, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Run != importID {
		t.Errorf("import job ran as run %s, not its argument's %s", imported.Run, importID)
	}
	var importRun model.ImportRun
	if err := db.Take(&importRun, "id = ?", imported.Run).Error; err != nil {
		t.Fatal(err)
	}
	if importRun.Status != model.RunSucceeded || importRun.InputVersion != imported.InputVersion || imported.InputVersion == "" {
		t.Errorf("import job recorded %+v for run %+v", imported, importRun)
	}

	var generated jobs.GenerateOutput
	genID := uuid.New()
	if err := run(jobs.GenerateArgs{Run: genID}, &generated); err != nil {
		t.Fatal(err)
	}
	var genRun model.GeneratorRun
	if err := db.Take(&genRun, "id = ?", generated.Run).Error; err != nil {
		t.Fatal(err)
	}
	if genRun.Status != model.RunSucceeded || genRun.Snapshot.Digest() != generated.Digest || generated.Run != genID ||
		len(generated.InputImportRuns) != 1 || generated.InputImportRuns[0] != imported.Run {
		t.Errorf("generate job recorded %+v for run %s (%s)", generated, genRun.ID, genRun.Status)
	}

	err = run(jobs.ImportArgs{Importer: "nope", Run: uuid.New()}, new(jobs.ImportOutput))
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("job of an unknown importer: %v, want its error", err)
	}
}

var config = jobs.Config{JobTimeout: time.Minute, RescueAfter: time.Hour}

// abandon records a running import run started before config.RescueAfter.
func abandon(t *testing.T, s *store.Store, db *gorm.DB) uuid.UUID {
	t.Helper()
	run, err := s.StartImport(context.Background(), uuid.New(), "test", model.LayerUpstream, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE import_runs SET started_at = ? WHERE id = ?`, time.Now().Add(-config.RescueAfter-time.Minute), run.ID).Error; err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func statusOf(t *testing.T, db *gorm.DB, id uuid.UUID) model.ImportRun {
	t.Helper()
	var r model.ImportRun
	if err := db.Take(&r, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	return r
}

// Abandoned runs are failed when a worker starts and by the periodic sweep.
func TestWorkersFailAbandonedRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sqlDB, db := dbtest.Fresh(t, "jobs_abandoned")
	dbtest.Migrate(t, sqlDB)
	if err := jobs.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	s := store.New(db)
	atStart := abandon(t, s, db)
	c, err := jobs.New(sqlDB, s, importers.NewRegistry(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Stop(context.Background()) })
	if r := statusOf(t, db, atStart); r.Status != model.RunFailed || r.Error != store.AbandonedError {
		t.Errorf("abandoned run after worker start: %s %q", r.Status, r.Error)
	}

	later := abandon(t, s, db)
	res, err := c.Insert(ctx, jobs.FailAbandonedArgs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Job.Queue != jobs.Queue || res.Job.MaxAttempts != 1 {
		t.Errorf("sweep job on queue %q with %d attempts", res.Job.Queue, res.Job.MaxAttempts)
	}
	if _, err := jobs.Wait(ctx, c, res.Job.ID); err != nil {
		t.Fatal(err)
	}
	if r := statusOf(t, db, later); r.Status != model.RunFailed || r.Error != store.AbandonedError {
		t.Errorf("abandoned run after the sweep job: %s %q", r.Status, r.Error)
	}
}

// blocking is an importer that runs until its context ends.
type blocking struct{}

func (blocking) Name() string { return "blocking" }
func (blocking) Origin() importers.Origin {
	return importers.Origin{Layer: model.LayerUpstream, Source: "blocking"}
}
func (blocking) Run(ctx context.Context, _ *store.Store, _ uuid.UUID) (importers.Summary, error) {
	<-ctx.Done()
	return importers.Summary{}, ctx.Err()
}

// A job past Config.JobTimeout is cancelled, its run failed, not retried.
func TestJobTimeoutFailsTheRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sqlDB, db := dbtest.Fresh(t, "jobs_timeout")
	dbtest.Migrate(t, sqlDB)
	if err := jobs.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	c, err := jobs.New(sqlDB, store.New(db), importers.NewRegistry(blocking{}), jobs.Config{JobTimeout: 300 * time.Millisecond, RescueAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Stop(context.Background()) })
	id := uuid.New()
	res, err := c.Insert(ctx, jobs.ImportArgs{Importer: "blocking", Run: id}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Wait(ctx, c, res.Job.ID); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("job past its timeout: %v, want deadline exceeded", err)
	}
	if r := statusOf(t, db, id); r.Status != model.RunFailed || !strings.Contains(r.Error, "deadline exceeded") {
		t.Errorf("run of the timed-out job: %s %q", r.Status, r.Error)
	}
}

// Shutdown cancels a run still going after grace and records it failed.
func TestShutdownCancelsRunningRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sqlDB, db := dbtest.Fresh(t, "jobs_shutdown")
	dbtest.Migrate(t, sqlDB)
	if err := jobs.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	c, err := jobs.New(sqlDB, store.New(db), importers.NewRegistry(blocking{}), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := c.Insert(ctx, jobs.ImportArgs{Importer: "blocking", Run: id}, nil); err != nil {
		t.Fatal(err)
	}
	for {
		var n int64
		if err := db.Model(&model.ImportRun{}).Where("id = ?", id).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("the import never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	if err := c.Shutdown(200 * time.Millisecond); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("shutdown took %s", d)
	}
	if r := statusOf(t, db, id); r.Status != model.RunFailed || !strings.Contains(r.Error, "context canceled") {
		t.Errorf("run cancelled by shutdown: %s %q", r.Status, r.Error)
	}
}
