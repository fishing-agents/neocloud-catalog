// Package importers runs importers: each fills a layer of the store from one
// source as an import run, recorded with its outcome and diff summary.
package importers

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Importer fills a layer of the store from one source.
type Importer interface {
	// Name is the importer's registry key and the importer of its runs.
	Name() string
	// Origin is what its runs write: their layer and the provenance they
	// stamp on the rows (R13).
	Origin() Origin
	// Run writes the importer's input through s as the running import run.
	Run(ctx context.Context, s *store.Store, run uuid.UUID) (Summary, error)
}

// Origin is the layer an importer writes and the source and source URL its
// runs record.
type Origin struct {
	Layer, Source, SourceURL string
}

// Summary is an import run's result: the version of the input it read, kept
// in import_runs.input_version, and the rows it changed per table.
type Summary struct {
	InputVersion string        `json:"-"`
	Changes      store.Changes `json:"changes"`
}

// Registry holds importers by name.
type Registry map[string]Importer

// NewRegistry registers imps; it panics on two importers of one name.
func NewRegistry(imps ...Importer) Registry {
	r := Registry{}
	for _, imp := range imps {
		if r[imp.Name()] != nil {
			panic("importers: two importers named " + imp.Name())
		}
		r[imp.Name()] = imp
	}
	return r
}

// Execute runs imp as a new import run and records how it ended, failed runs
// included.
func Execute(ctx context.Context, s *store.Store, imp Importer) (model.ImportRun, error) {
	o := imp.Origin()
	run, err := s.StartImport(ctx, imp.Name(), o.Layer, o.Source, o.SourceURL)
	if err != nil {
		return run, err
	}
	sum, runErr := imp.Run(ctx, s, run.ID)
	// A cancelled run is still recorded as failed.
	finished, err := s.FinishImport(context.WithoutCancel(ctx), run.ID, sum.InputVersion, sum, runErr)
	if err != nil {
		return run, errors.Join(runErr, err)
	}
	return finished, runErr
}
