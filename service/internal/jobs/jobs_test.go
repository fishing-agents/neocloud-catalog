package jobs_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Import and generate jobs on the catalog queue run the importer and the
// generator and record their runs; a job that fails reports its error.
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
	c, err := jobs.New(sqlDB, store.New(db), importers.NewRegistry(imp))
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
	if err := run(jobs.ImportArgs{Importer: "bootstrap-from-json"}, &imported); err != nil {
		t.Fatal(err)
	}
	var importRun model.ImportRun
	if err := db.Take(&importRun, "id = ?", imported.Run).Error; err != nil {
		t.Fatal(err)
	}
	if importRun.Status != model.RunSucceeded || importRun.InputVersion != imported.InputVersion || imported.InputVersion == "" {
		t.Errorf("import job recorded %+v for run %+v", imported, importRun)
	}

	var generated jobs.GenerateOutput
	if err := run(jobs.GenerateArgs{}, &generated); err != nil {
		t.Fatal(err)
	}
	var genRun model.GeneratorRun
	if err := db.Take(&genRun, "id = ?", generated.Run).Error; err != nil {
		t.Fatal(err)
	}
	if genRun.Status != model.RunSucceeded || genRun.Snapshot.Digest() != generated.Digest ||
		len(generated.InputImportRuns) != 1 || generated.InputImportRuns[0] != imported.Run {
		t.Errorf("generate job recorded %+v for run %s (%s)", generated, genRun.ID, genRun.Status)
	}

	err = run(jobs.ImportArgs{Importer: "nope"}, new(jobs.ImportOutput))
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("job of an unknown importer: %v, want its error", err)
	}
}
