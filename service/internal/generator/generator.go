// Package generator runs the core generator over the effective catalog as a
// generator run and stores what it derives.
package generator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Run generates the snapshot of the effective catalog as a new generator run
// and stores the run: its inputs' digest and import runs, the report, the
// snapshot generated at the run's start (its version is a release's to set)
// and the fits, estimates and guidance it derives. A failed generation is
// stored as a failed run and returned with its error.
func Run(ctx context.Context, s *store.Store) (model.GeneratorRun, error) {
	run, err := s.StartGeneratorRun(ctx)
	if err != nil {
		return run, err
	}
	genErr := generate(ctx, s, &run)
	run.Status = model.RunSucceeded
	if genErr != nil {
		run.Status, run.Error = model.RunFailed, genErr.Error()
	}
	// A cancelled run is still recorded as failed.
	ctx = context.WithoutCancel(ctx)
	err = s.FinishGeneratorRun(ctx, &run)
	if err != nil && genErr == nil {
		genErr = err
		run.Status, run.Error, run.Snapshot = model.RunFailed, err.Error(), nil
		err = s.FinishGeneratorRun(ctx, &run)
	}
	return run, errors.Join(genErr, err)
}

func generate(ctx context.Context, s *store.Store, run *model.GeneratorRun) error {
	in, runs, err := s.EffectiveWithRuns(ctx)
	if err != nil {
		return err
	}
	run.InputDigest, run.InputImportRuns = InputDigest(in), runs
	snap, report, err := catalog.Generate(in.Families, in.NodeTypes, in.Models, in.Drafts, in.Backends)
	if err != nil {
		return err
	}
	snap.GeneratedAt = run.StartedAt
	run.Snapshot, run.Report = &snap, report
	return nil
}

// InputDigest is the sha256 of the curated inputs Generate reads, in order:
// Generate is deterministic, so equal digests give equal snapshots.
func InputDigest(in store.Inputs) string {
	h := sha256.New()
	_ = json.NewEncoder(h).Encode([]any{in.Families, in.NodeTypes, in.Models, in.Drafts, in.Backends})
	return hex.EncodeToString(h.Sum(nil))
}
