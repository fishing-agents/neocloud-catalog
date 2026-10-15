// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package broker is the model-source broker: it searches and inspects source repositories and imports
// one repository into the local layer, or into a tenant's layer with the tenant's token. Hugging Face
// is the only source served.
package broker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/huggingface"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

var (
	// ErrUnsupported reports a source the broker does not serve.
	ErrUnsupported = errors.New("unsupported source: modelscope is not supported yet; use huggingface")
	// ErrRateLimited reports a spent upstream budget; retry after RetryAfter.
	ErrRateLimited = errors.New("the upstream budget is spent; retry later")
)

const (
	// SearchTTL is how long a search answers from the cache.
	SearchTTL = 10 * time.Minute
	// RetryAfter is when a rate-limited caller may retry.
	RetryAfter = 10 * time.Second
	// ImporterName names the import runs of local model imports.
	ImporterName = "model-import"

	// Upstream budget of searches and inspections (each a few Hub requests); the Hub allows anonymous
	// clients 500 requests per five minutes.
	upstreamRate  = rate.Limit(0.5)
	upstreamBurst = 10
	maxCached     = 1000
)

// Broker serves the sources and runs model imports into s.
type Broker struct {
	s       *store.Store
	hub     *huggingface.Importer
	limiter *rate.Limiter

	mu    sync.Mutex
	cache map[searchKey]cachedSearch
	now   func() time.Time
}

type searchKey struct {
	source, q string
	limit     int
}

type cachedSearch struct {
	hits    []huggingface.Hit
	expires time.Time
}

// New returns the broker of hub (its token the operator's) writing into s.
func New(s *store.Store, hub *huggingface.Importer) *Broker {
	return &Broker{s: s, hub: hub, limiter: rate.NewLimiter(upstreamRate, upstreamBurst), cache: map[searchKey]cachedSearch{}, now: time.Now}
}

func checkSource(source string) error {
	if source != catalog.SourceHuggingFace {
		return fmt.Errorf("%w (got %q)", ErrUnsupported, source)
	}
	return nil
}

// upstream spends one unit of the upstream budget.
func (b *Broker) upstream() error {
	if !b.limiter.Allow() {
		return ErrRateLimited
	}
	return nil
}

// Search lists source's public repositories matching q, from the cache for SearchTTL.
func (b *Broker) Search(ctx context.Context, source, q string, limit int) ([]huggingface.Hit, error) {
	if err := checkSource(source); err != nil {
		return nil, err
	}
	key := searchKey{source, q, limit}
	b.mu.Lock()
	c, ok := b.cache[key]
	b.mu.Unlock()
	if ok && b.now().Before(c.expires) {
		return c.hits, nil
	}
	if err := b.upstream(); err != nil {
		return nil, err
	}
	hits, err := b.hub.Search(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if len(b.cache) >= maxCached {
		for k, c := range b.cache {
			if !now.Before(c.expires) {
				delete(b.cache, k)
			}
		}
		if len(b.cache) >= maxCached {
			clear(b.cache)
		}
	}
	b.cache[key] = cachedSearch{hits, now.Add(SearchTTL)}
	return hits, nil
}

// Inspect reads source's repo at revision with the operator's token; a private repository is
// huggingface.ErrNotFound, so tenants cannot look into the operator's account.
func (b *Broker) Inspect(ctx context.Context, source, repo, revision string) (*huggingface.Repository, error) {
	if err := checkSource(source); err != nil {
		return nil, err
	}
	if err := b.upstream(); err != nil {
		return nil, err
	}
	r, err := b.hub.Read(ctx, repo, revision, "")
	if err == nil && r.Private {
		return nil, fmt.Errorf("%s: %w", repo, huggingface.ErrNotFound)
	}
	return r, err
}

// built is a repository made a catalog model: template is what the local layer stores, generated the
// model with its derived fields, fits its fits.
type built struct {
	commit    string
	template  catalog.ModelTemplate
	generated catalog.ModelTemplate
	fits      []catalog.ModelFit
	report    []string
}

// build reads mi's repository (with token, else the operator's) and generates its model against the
// effective catalog's families, node types and backends, which also validates it.
func (b *Broker) build(ctx context.Context, mi model.ModelImport, token string) (built, error) {
	r, err := b.hub.Read(ctx, mi.Repo, mi.Revision, token)
	if err != nil {
		return built{}, err
	}
	out := built{commit: r.Commit}
	in, err := b.s.Effective(ctx)
	if err != nil {
		return out, err
	}
	if out.template, err = r.Template(in.Backends); err != nil {
		return out, err
	}
	snap, report, err := catalog.Generate(in.Families, in.NodeTypes, []catalog.ModelTemplate{out.template}, nil, in.Backends)
	if err != nil {
		return out, fmt.Errorf("generate: %w", err)
	}
	out.generated, out.fits, out.report = snap.Models[0], snap.Fits, report
	return out, nil
}

// finish records mi's end from res and err; tm is a private import's tenant model.
func (b *Broker) finish(ctx context.Context, mi model.ModelImport, res built, err error, tm *model.TenantModel) (model.ModelImport, error) {
	mi.State, mi.Commit, mi.Report = model.ImportSucceeded, res.commit, res.report
	if err != nil {
		mi.State, mi.Error, mi.Report = model.ImportFailed, err.Error(), nil
	} else {
		mi.ModelSlug = res.template.Slug
	}
	return b.s.FinishModelImport(context.WithoutCancel(ctx), mi, tm)
}

// Run runs the queued local import id as the import run of the same id; an import not queued (taken
// by another worker) is left alone. A failed import is recorded and returned.
func (b *Broker) Run(ctx context.Context, id uuid.UUID) error {
	mi, err := b.s.StartModelImport(ctx, id)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	imp := &localImport{b: b, mi: mi}
	_, runErr := importers.Execute(ctx, b.s, imp, id)
	if _, err := b.finish(ctx, mi, imp.res, runErr, nil); err != nil {
		return errors.Join(runErr, err)
	}
	return runErr
}

// localImport is the import run writing a model import's model to the local layer.
type localImport struct {
	b   *Broker
	mi  model.ModelImport
	res built
}

func (*localImport) Name() string { return ImporterName }

func (l *localImport) Origin() importers.Origin {
	return importers.Origin{Layer: model.LayerLocal, Source: l.mi.Source, SourceURL: l.b.hub.Endpoint()}
}

func (l *localImport) Run(ctx context.Context, s *store.Store, run uuid.UUID) (importers.Summary, error) {
	var err error
	l.res, err = l.b.build(ctx, l.mi, "")
	sum := importers.Summary{InputVersion: l.mi.Repo + "@" + l.res.commit, Sources: &importers.SourceCounts{Read: 1}}
	if err != nil {
		sum.Sources = &importers.SourceCounts{Failed: 1}
		return sum, err
	}
	sum.Changes, sum.Conflicts, err = s.ImportModel(ctx, run, l.res.template)
	return sum, err
}

// RunPrivate runs the queued private import id with the tenant's token into its tenant's layer and
// returns it finished. The token lives in this call only: it is never stored, logged or queued.
func (b *Broker) RunPrivate(ctx context.Context, id uuid.UUID, token string) (model.ModelImport, error) {
	mi, err := b.s.StartModelImport(ctx, id)
	if err != nil {
		return mi, err
	}
	if !mi.Private || token == "" {
		return b.finish(ctx, mi, built{}, errors.New("a private import needs its tenant's token"), nil)
	}
	res, err := b.build(ctx, mi, token)
	if err != nil {
		return b.finish(ctx, mi, res, err, nil)
	}
	tm := &model.TenantModel{TenantID: mi.TenantID, Slug: res.generated.Slug, Source: mi.Source, SourceURL: b.hub.Endpoint() + "/" + mi.Repo,
		AsOf: time.Now().UTC(), ModelImportID: mi.ID, Model: res.generated, Fits: res.fits}
	return b.finish(ctx, mi, res, nil, tm)
}
