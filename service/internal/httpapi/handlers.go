package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river/rivertype"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/release"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

var _ ServerInterface = (*handlers)(nil)

func (h *handlers) GetCatalog(w http.ResponseWriter, r *http.Request, params GetCatalogParams) {
	out := EffectiveCatalog{SchemaVersion: catalog.SchemaVersion}
	var in store.Inputs
	var err error
	if params.Provenance != nil && *params.Provenance {
		var p store.Provenances
		in, p, err = h.Store.EffectiveProvenance(r.Context())
		out.Provenance = &CatalogProvenance{Families: provenance(p[store.KindFamily]), NodeTypes: provenance(p[store.KindNodeType]),
			Models: provenance(p[store.KindModel]), Quantizations: provenance(p[store.KindQuantization]),
			DraftModels: provenance(p[store.KindDraft]), Backends: provenance(p[store.KindBackend])}
	} else {
		in, err = h.Store.Effective(r.Context())
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	out.Families, out.NodeTypes, out.Models = orEmpty(in.Families), orEmpty(in.NodeTypes), orEmpty(in.Models)
	out.DraftModels, out.Backends = orEmpty(in.Drafts), orEmpty(in.Backends)
	writeJSON(w, http.StatusOK, out)
}

func provenance(byKey map[string][]store.ProvenanceEntry) ProvenanceByKey {
	out := make(ProvenanceByKey, len(byKey))
	for key, entries := range byKey {
		ps := make([]Provenance, len(entries))
		for i, e := range entries {
			ps[i] = Provenance{Layer: Layer(e.Layer), Source: e.Source, SourceUrl: e.SourceURL, AsOf: e.AsOf, ImportRunId: e.ImportRunID,
				Effective: e.Effective, Fields: e.Fields}
		}
		out[key] = ps
	}
	return out
}

func (h *handlers) GetLocalModel(w http.ResponseWriter, r *http.Request, slug string) {
	m, err := h.Store.LocalModel(r.Context(), slug)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *handlers) PutLocalModel(w http.ResponseWriter, r *http.Request, slug string) {
	var m ModelTemplate
	if !decode(w, r, &m) {
		return
	}
	if m.Slug != slug {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("slug %q differs from the path's %q", m.Slug, slug))
		return
	}
	if err := h.Store.Upsert(r.Context(), m); err != nil {
		fail(w, r, err)
		return
	}
	h.GetLocalModel(w, r, slug)
}

func (h *handlers) DeleteLocalModel(w http.ResponseWriter, r *http.Request, slug string) {
	if err := h.Store.Delete(r.Context(), store.KindModel, slug); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func presetIndex(m *catalog.ModelTemplate, name string) int {
	return slices.IndexFunc(m.Presets, func(p catalog.Preset) bool { return p.Name == name })
}

func (h *handlers) GetLocalPreset(w http.ResponseWriter, r *http.Request, slug string, name string) {
	m, err := h.Store.LocalModel(r.Context(), slug)
	if err != nil {
		fail(w, r, err)
		return
	}
	i := presetIndex(&m, name)
	if i < 0 {
		fail(w, r, fmt.Errorf("local model %q has no preset %q: %w", slug, name, store.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, m.Presets[i])
}

func (h *handlers) PutLocalPreset(w http.ResponseWriter, r *http.Request, slug string, name string) {
	var p Preset
	if !decode(w, r, &p) {
		return
	}
	if p.Name != name {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("name %q differs from the path's %q", p.Name, name))
		return
	}
	err := h.Store.EditLocalModel(r.Context(), slug, func(m *catalog.ModelTemplate) error {
		if i := presetIndex(m, name); i >= 0 {
			m.Presets[i] = p
		} else {
			m.Presets = append(m.Presets, p)
		}
		return nil
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	h.GetLocalPreset(w, r, slug, name)
}

func (h *handlers) DeleteLocalPreset(w http.ResponseWriter, r *http.Request, slug string, name string) {
	err := h.Store.EditLocalModel(r.Context(), slug, func(m *catalog.ModelTemplate) error {
		i := presetIndex(m, name)
		if i < 0 {
			return fmt.Errorf("local model %q has no preset %q: %w", slug, name, store.ErrNotFound)
		}
		m.Presets = slices.Delete(m.Presets, i, i+1)
		return nil
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) GetLocalNodeType(w http.ResponseWriter, r *http.Request, id string) {
	nt, err := h.Store.LocalNodeType(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, nt)
}

func (h *handlers) PutLocalNodeType(w http.ResponseWriter, r *http.Request, id string) {
	var nt NodeType
	if !decode(w, r, &nt) {
		return
	}
	if nt.ID != id {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("id %q differs from the path's %q", nt.ID, id))
		return
	}
	if err := h.Store.Upsert(r.Context(), nt); err != nil {
		fail(w, r, err)
		return
	}
	h.GetLocalNodeType(w, r, id)
}

func (h *handlers) DeleteLocalNodeType(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.Store.Delete(r.Context(), store.KindNodeType, id); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) ListLocalPrices(w http.ResponseWriter, r *http.Request) {
	prices, err := h.Store.LocalPrices(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]Price, len(prices))
	for i, p := range prices {
		out[i] = Price{NodeTypeId: p.NodeTypeID, Region: p.Region, Billing: Billing(p.Billing), UsdPerHour: p.USDPerHour, AsOf: &p.AsOf}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) PutLocalPrices(w http.ResponseWriter, r *http.Request) {
	var in []Price
	if !decode(w, r, &in) {
		return
	}
	if in == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body: a list of prices required; DELETE removes them all")
		return
	}
	prices := make([]model.NodeTypePrice, len(in))
	for i, p := range in {
		prices[i] = model.NodeTypePrice{NodeTypeID: p.NodeTypeId, Region: p.Region, Billing: string(p.Billing), USDPerHour: p.UsdPerHour}
	}
	if err := h.Store.ReplaceLocalPrices(r.Context(), prices); err != nil {
		fail(w, r, err)
		return
	}
	h.ListLocalPrices(w, r)
}

func (h *handlers) DeleteLocalPrices(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.ReplaceLocalPrices(r.Context(), nil); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) StartImport(w http.ResponseWriter, r *http.Request) {
	var in ImportCreate
	if !decode(w, r, &in) {
		return
	}
	if in.Importer == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "importer required")
		return
	}
	imp := h.Importers[in.Importer]
	if imp == nil {
		fail(w, r, fmt.Errorf("importer %q: %w", in.Importer, store.ErrNotFound))
		return
	}
	run := uuid.New()
	if _, err := h.Jobs.Insert(r.Context(), jobs.ImportArgs{Importer: in.Importer, Run: run}, nil); err != nil {
		fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/imports/"+run.String())
	writeJSON(w, http.StatusAccepted, queuedImport(run, imp))
}

// queuedImport is the import run id of imp before a worker records it.
func queuedImport(id uuid.UUID, imp importers.Importer) ImportRun {
	o := imp.Origin()
	return ImportRun{Id: id, Importer: imp.Name(), Layer: Layer(o.Layer), Source: o.Source, SourceUrl: o.SourceURL, Status: Queued,
		Summary: ImportSummary{Changes: map[string]Change{}}}
}

func (h *handlers) ListImports(w http.ResponseWriter, r *http.Request, params ListImportsParams) {
	limit := 100
	if params.Limit != nil {
		limit = *params.Limit
	}
	if limit < 1 || limit > 1000 {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be 1 to 1000")
		return
	}
	var importer string
	if params.Importer != nil {
		importer = *params.Importer
	}
	// Queued jobs first: a job started meanwhile then shows as its run.
	queued, err := h.Jobs.Queued(r.Context(), jobs.ImportArgs{}.Kind())
	if err != nil {
		fail(w, r, err)
		return
	}
	runs, err := h.Store.ImportRuns(r.Context(), importer, limit)
	if err != nil {
		fail(w, r, err)
		return
	}
	recorded := map[uuid.UUID]bool{}
	for _, run := range runs {
		recorded[run.ID] = true
	}
	out := []ImportRun{}
	for _, job := range queued {
		var args jobs.ImportArgs
		if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
			fail(w, r, err)
			return
		}
		if imp := h.Importers[args.Importer]; imp != nil && !recorded[args.Run] && (importer == "" || args.Importer == importer) {
			out = append(out, queuedImport(args.Run, imp))
		}
	}
	for _, run := range runs {
		ir, err := importRun(run)
		if err != nil {
			fail(w, r, err)
			return
		}
		out = append(out, ir)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) GetImport(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	run, err := h.Store.ImportRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		job, err := h.job(r.Context(), jobs.ImportArgs{}.Kind(), id)
		if err != nil {
			fail(w, r, err)
			return
		}
		var args jobs.ImportArgs
		if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
			fail(w, r, err)
			return
		}
		imp := h.Importers[args.Importer]
		if imp == nil {
			fail(w, r, fmt.Errorf("import run %s of importer %q: %w", id, args.Importer, store.ErrNotFound))
			return
		}
		out := queuedImport(id, imp)
		out.Status, out.Error, out.FinishedAt = pending(job)
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	out, err := importRun(run)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// job returns the job of kind running run, which no row records yet.
func (h *handlers) job(ctx context.Context, kind string, run uuid.UUID) (*rivertype.JobRow, error) {
	job, err := h.Jobs.Job(ctx, kind, run)
	if err == nil && job == nil {
		err = fmt.Errorf("%s run %s: %w", kind, run, store.ErrNotFound)
	}
	return job, err
}

// pending is the state of a run no row records yet: queued until a worker
// starts it, failed if its job ended without starting it.
func pending(job *rivertype.JobRow) (RunStatus, string, *time.Time) {
	if job.State != rivertype.JobStateDiscarded && job.State != rivertype.JobStateCancelled {
		return Queued, "", nil
	}
	msg := "job " + string(job.State)
	if n := len(job.Errors); n > 0 {
		msg = job.Errors[n-1].Error
	}
	return Failed, msg, job.FinalizedAt
}

// importRun is run with its summary's changes {} rather than absent.
func importRun(run model.ImportRun) (ImportRun, error) {
	var sum ImportSummary
	if len(run.Summary) > 0 {
		if err := json.Unmarshal(run.Summary, &sum); err != nil {
			return ImportRun{}, fmt.Errorf("import run %s summary: %w", run.ID, err)
		}
	}
	if sum.Changes == nil {
		sum.Changes = map[string]Change{}
	}
	return ImportRun{Id: run.ID, Importer: run.Importer, Layer: Layer(run.Layer), Source: run.Source, SourceUrl: run.SourceURL,
		Status: RunStatus(run.Status), InputVersion: run.InputVersion, StartedAt: &run.StartedAt, FinishedAt: run.FinishedAt,
		Summary: sum, Error: run.Error}, nil
}

func (h *handlers) StartGeneratorRun(w http.ResponseWriter, r *http.Request) {
	run := uuid.New()
	if _, err := h.Jobs.Insert(r.Context(), jobs.GenerateArgs{Run: run}, nil); err != nil {
		fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/generator-runs/"+run.String())
	writeJSON(w, http.StatusAccepted, queuedGeneratorRun(run))
}

func queuedGeneratorRun(id uuid.UUID) GeneratorRun {
	return GeneratorRun{Id: id, Status: Queued, InputImportRuns: []uuid.UUID{}, SchemaVersion: catalog.SchemaVersion, Report: []string{}}
}

func (h *handlers) GetGeneratorRun(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	run, err := h.Store.GeneratorRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		job, err := h.job(r.Context(), jobs.GenerateArgs{}.Kind(), id)
		if err != nil {
			fail(w, r, err)
			return
		}
		out := queuedGeneratorRun(id)
		out.Status, out.Error, out.FinishedAt = pending(job)
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, GeneratorRun{Id: run.ID, Status: RunStatus(run.Status), StartedAt: &run.StartedAt, FinishedAt: run.FinishedAt,
		InputDigest: run.InputDigest, InputImportRuns: orEmpty([]uuid.UUID(run.InputImportRuns)), SchemaVersion: run.SchemaVersion,
		Report: orEmpty([]string(run.Report)), Error: run.Error})
}

func toRelease(rel model.Release) Release {
	return Release{Id: rel.ID, Version: rel.Version, Digest: rel.Digest, SchemaVersion: rel.SchemaVersion, GeneratorRun: rel.GeneratorRunID,
		ArtifactRef: rel.ArtifactRef, PublishedAt: rel.PublishedAt, CreatedAt: rel.CreatedAt}
}

func (h *handlers) ListReleases(w http.ResponseWriter, r *http.Request) {
	rels, err := h.Store.Releases(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]Release, len(rels))
	for i, rel := range rels {
		out[i] = toRelease(rel)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) CreateRelease(w http.ResponseWriter, r *http.Request) {
	var in ReleaseCreate
	if !decode(w, r, &in) {
		return
	}
	rel, err := release.Create(r.Context(), h.Store, in.GeneratorRun)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRelease(rel.Release))
}

func (h *handlers) PublishRelease(w http.ResponseWriter, r *http.Request, version string) {
	if h.Publish == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "publishing needs release.ref and release.key")
		return
	}
	// Pushing and signing outlast server.write_timeout; publishing again is a
	// no-op, so a client that lost the answer may retry.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(h.PublishTimeout + publishAnswerTime))
	ctx, cancel := context.WithTimeout(r.Context(), h.PublishTimeout)
	defer cancel()
	rel, err := release.Get(ctx, h.Store, version)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := release.Publish(ctx, rel, *h.Publish); err != nil {
		fail(w, r, err)
		return
	}
	if rel, err = release.Get(ctx, h.Store, version); err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRelease(rel.Release))
}

func (h *handlers) ListCostEstimates(w http.ResponseWriter, r *http.Request, params ListCostEstimatesParams) {
	var f store.CostFilter
	if params.Billing != nil {
		if !params.Billing.Valid() {
			writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("billing %q, want on_demand, spot or reserved_1y", *params.Billing))
			return
		}
		f.Billing = string(*params.Billing)
	}
	if params.Model != nil {
		f.Model = *params.Model
	}
	if params.Region != nil {
		f.Region = *params.Region
	}
	rows, err := h.Store.CostEstimates(r.Context(), f)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]CostEstimate, len(rows))
	for i, c := range rows {
		out[i] = CostEstimate{Model: c.ModelSlug, Setup: CostEstimateSetup(c.Setup), FitKey: c.FitKey, NodeTypeId: c.NodeTypeID, Region: c.Region,
			Billing: Billing(c.Billing), Instances: c.Instances, UsdPerHour: c.USDPerHour, UsdPerMonth: c.USDPerMonth, PriceSource: c.PriceSource,
			PriceSourceUrl: c.PriceSourceURL, PriceAsOf: c.PriceAsOf, GeneratorRun: c.GeneratorRunID, Cheapest: c.Cheapest}
	}
	writeJSON(w, http.StatusOK, out)
}
