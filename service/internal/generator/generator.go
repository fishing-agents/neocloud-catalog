// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package generator runs the core generator over the effective catalog and stores the result.
package generator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/costs"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Run generates the effective catalog's snapshot as generator run id and stores it with its derived
// rows and cost estimates. A failed generation is stored as a failed run; the error names the run.
func Run(ctx context.Context, s *store.Store, id uuid.UUID) (model.GeneratorRun, error) {
	run, err := s.StartGeneratorRun(ctx, id)
	if err != nil {
		return run, fmt.Errorf("start generator run: %w", err)
	}
	estimates, genErr := generate(ctx, s, &run)
	run.Status = model.RunSucceeded
	if genErr != nil {
		run.Status, run.Error = model.RunFailed, genErr.Error()
	}
	// A cancelled run is still recorded as failed.
	ctx = context.WithoutCancel(ctx)
	err = s.FinishGeneratorRun(ctx, &run, estimates)
	if err != nil && genErr == nil {
		genErr = err
		run.Status, run.Error, run.Snapshot = model.RunFailed, err.Error(), nil
		err = s.FinishGeneratorRun(ctx, &run, nil)
	}
	if err := errors.Join(genErr, err); err != nil {
		return run, fmt.Errorf("generator run %s: %w", run.ID, err)
	}
	return run, nil
}

// generate fills run and returns its cost estimates. Prices feed only those internal estimates, so
// the input digest and import runs leave them out.
func generate(ctx context.Context, s *store.Store, run *model.GeneratorRun) ([]model.CostEstimate, error) {
	in, runs, err := s.EffectiveWithRuns(ctx)
	if err != nil {
		return nil, err
	}
	run.InputDigest, run.InputImportRuns = InputDigest(in), runs
	snap, report, err := catalog.Generate(in.Families, in.NodeTypes, in.Models, in.Drafts, in.Backends)
	if err != nil {
		return nil, err
	}
	prices, err := s.EffectivePrices(ctx)
	if err != nil {
		return nil, err
	}
	snap.GeneratedAt = run.StartedAt
	run.Snapshot, run.Report = &snap, report
	return costs.Estimate(run.ID, &snap, prices), nil
}

// InputDigest is the sha256 of the inputs Generate reads; Generate is deterministic, so equal
// digests give equal snapshots.
func InputDigest(in store.Inputs) string {
	h := sha256.New()
	_ = json.NewEncoder(h).Encode([]any{in.Families, in.NodeTypes, in.Models, in.Drafts, in.Backends})
	return hex.EncodeToString(h.Sum(nil))
}
