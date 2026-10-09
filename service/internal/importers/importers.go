// Package importers runs importers, each filling a store layer from one source
// as a recorded import run.
package importers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Importer fills a layer of the store from one source.
type Importer interface {
	Name() string
	// Origin is the layer its runs write and the provenance they stamp.
	Origin() Origin
	Run(ctx context.Context, s *store.Store, run uuid.UUID) (Summary, error)
}

// Origin is the layer an importer writes and the provenance its runs record.
type Origin struct {
	Layer, Source, SourceURL string
}

// Summary is an import run's result. Conflicts are imported values curated ones
// were kept over; Errors are input parts a run went on without.
type Summary struct {
	InputVersion string           `json:"-"`
	Changes      store.Changes    `json:"changes"`
	Conflicts    []store.Conflict `json:"conflicts,omitempty"`
	Errors       []string         `json:"errors,omitempty"`
	Sources      *SourceCounts    `json:"sources,omitempty"`
}

// SourceCounts counts sources read in full and failed; a succeeded run may have
// failed sources.
type SourceCounts struct {
	Read   int `json:"read"`
	Failed int `json:"failed"`
}

// Registry holds a deployment's importers by name and the names it refuses.
type Registry struct {
	imps    map[string]Importer
	refused map[string]string
}

// NewRegistry panics on duplicate names.
func NewRegistry(imps ...Importer) Registry {
	r := Registry{imps: map[string]Importer{}, refused: map[string]string{}}
	for _, imp := range imps {
		if r.imps[imp.Name()] != nil {
			panic("importers: two importers named " + imp.Name())
		}
		r.imps[imp.Name()] = imp
	}
	return r
}

// Refuse makes Lookup refuse name with why.
func (r Registry) Refuse(name, why string) {
	delete(r.imps, name)
	r.refused[name] = why
}

// Lookup returns store.ErrConflict for a refused name and store.ErrNotFound for
// an unknown one.
func (r Registry) Lookup(name string) (Importer, error) {
	if why, ok := r.refused[name]; ok {
		return nil, fmt.Errorf("%w: importer %q is not run here: %s", store.ErrConflict, name, why)
	}
	if imp := r.imps[name]; imp != nil {
		return imp, nil
	}
	return nil, fmt.Errorf("importer %q: %w", name, store.ErrNotFound)
}

// Names returns the importer names sorted.
func (r Registry) Names() []string { return slices.Sorted(maps.Keys(r.imps)) }

// Execute runs imp as import run id and records its end, failures included.
func Execute(ctx context.Context, s *store.Store, imp Importer, id uuid.UUID) (model.ImportRun, error) {
	o := imp.Origin()
	run, err := s.StartImport(ctx, id, imp.Name(), o.Layer, o.Source, o.SourceURL)
	if err != nil {
		return run, fmt.Errorf("start %s import run: %w", imp.Name(), err)
	}
	sum, err := imp.Run(ctx, s, run.ID)
	finished, finishErr := s.FinishImport(context.WithoutCancel(ctx), run.ID, sum.InputVersion, sum, err)
	if finishErr != nil {
		finished, err = run, errors.Join(err, finishErr)
	}
	if err != nil {
		return finished, fmt.Errorf("import run %s: %w", run.ID, err)
	}
	return finished, nil
}
