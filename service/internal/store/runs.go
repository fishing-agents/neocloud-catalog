package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

// now is the current time as Postgres stores it, to the microsecond, so a
// run's start reads back as written.
func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// StartImport records a running import run of importer, started now, that
// writes layer with source and sourceURL.
func (s *Store) StartImport(ctx context.Context, importer, layer, source, sourceURL string) (model.ImportRun, error) {
	run := model.ImportRun{Importer: importer, Layer: layer, Source: source, SourceURL: sourceURL, Status: model.RunRunning, StartedAt: now()}
	return run, s.db.WithContext(ctx).Create(&run).Error
}

// FinishImport ends the running import run run with the version of the input
// it read and its summary: failed with runErr, else succeeded.
func (s *Store) FinishImport(ctx context.Context, run uuid.UUID, inputVersion string, summary any, runErr error) (model.ImportRun, error) {
	b, err := json.Marshal(summary)
	if err != nil {
		return model.ImportRun{}, err
	}
	status, msg := model.RunSucceeded, ""
	if runErr != nil {
		status, msg = model.RunFailed, runErr.Error()
	}
	var r model.ImportRun
	res := s.db.WithContext(ctx).Model(&r).Clauses(clause.Returning{}).Where("id = ? AND status = ?", run, model.RunRunning).
		Updates(map[string]any{"status": status, "finished_at": now(), "input_version": inputVersion, "summary": string(b), "error": msg})
	if res.Error == nil && res.RowsAffected == 0 {
		return r, fmt.Errorf("import run %s is not running", run)
	}
	return r, res.Error
}

// EffectiveWithRuns is Effective plus the import runs of its core values,
// read on one snapshot (see importRuns).
func (s *Store) EffectiveWithRuns(ctx context.Context) (Inputs, []uuid.UUID, error) {
	var in Inputs
	var runs []uuid.UUID
	err := s.read(ctx, func(tx *gorm.DB) error {
		up, local, payloads, err := loadEffective(tx)
		if err != nil {
			return err
		}
		if in, err = merge(up, local, payloads); err != nil {
			return err
		}
		runs = importRuns(up, local)
		return nil
	})
	return in, runs, err
}

// importRuns lists, sorted, the import runs of the effective core rows and of
// their values (field provenance): per object the rows of the layer it is
// taken from. Overlay payloads are not core inputs.
func importRuns(up, local rowSet) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	add := func(row any) {
		m := meta(row)
		if id := m.prov.ImportRunID; id != nil {
			seen[*id] = true
		}
		if m.fields != nil {
			for _, f := range m.fields.Data() {
				if f.ImportRunID != nil {
					seen[*f.ImportRunID] = true
				}
			}
		}
	}
	held := map[string]bool{}
	local.each(func(object string, row any) {
		held[object] = true
		add(row)
	})
	up.each(func(object string, row any) {
		if !held[object] {
			add(row)
		}
	})
	runs := slices.Collect(maps.Keys(seen))
	slices.SortFunc(runs, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return runs
}

// each calls fn with every core row of rs and the object it belongs to.
func (rs rowSet) each(fn func(object string, row any)) {
	for i := range rs.families {
		fn(keyOf(KindFamily, rs.families[i].Name), &rs.families[i])
	}
	for i := range rs.nodeTypes {
		fn(keyOf(KindNodeType, rs.nodeTypes[i].NodeTypeID), &rs.nodeTypes[i])
	}
	for i := range rs.models {
		fn(keyOf(KindModel, rs.models[i].Slug), &rs.models[i])
	}
	for i := range rs.quants {
		fn(keyOf(KindModel, rs.quants[i].ModelSlug), &rs.quants[i])
	}
	for i := range rs.presets {
		fn(keyOf(KindModel, rs.presets[i].ModelSlug), &rs.presets[i])
	}
	for i := range rs.sources {
		src := &rs.sources[i]
		object := keyOf(KindModel, src.ModelSlug)
		if src.DraftSlug != "" {
			object = keyOf(KindDraft, src.DraftSlug)
		}
		fn(object, src)
		for j := range rs.files[i] {
			fn(object, &rs.files[i][j])
		}
	}
	for i := range rs.drafts {
		fn(keyOf(KindDraft, rs.drafts[i].Slug), &rs.drafts[i])
	}
	for i := range rs.backends {
		fn(keyOf(KindBackend, rs.backends[i].Name), &rs.backends[i])
	}
	for i := range rs.versions {
		fn(keyOf(KindBackend, rs.versions[i].Backend), &rs.versions[i])
	}
	for i := range rs.flags {
		fn(keyOf(KindBackend, rs.flags[i].Backend), &rs.flags[i])
	}
}

// StartGeneratorRun records a running generator run of the core's schema
// version, started now.
func (s *Store) StartGeneratorRun(ctx context.Context) (model.GeneratorRun, error) {
	run := model.GeneratorRun{Status: model.RunRunning, StartedAt: now(), SchemaVersion: catalog.SchemaVersion}
	return run, s.db.WithContext(ctx).Create(&run).Error
}

// FinishGeneratorRun stores the running generator run run as it ended, now,
// in one transaction with the fits, fit estimates and guidance of its
// snapshot, if it has one.
func (s *Store) FinishGeneratorRun(ctx context.Context, run *model.GeneratorRun) error {
	t := now()
	run.FinishedAt = &t
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(run).Where("status = ?", model.RunRunning).
			Select("status", "finished_at", "input_digest", "input_import_runs", "schema_version", "report", "snapshot", "error").
			Updates(run)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("generator run %s is not running", run.ID)
		}
		if run.Snapshot == nil {
			return nil
		}
		fits, estimates, guidance := derive(run.ID, run.Snapshot)
		if err := insert(tx, fits); err != nil {
			return err
		}
		if err := insert(tx, estimates); err != nil {
			return err
		}
		return insert(tx, guidance)
	})
}

func insert[T any](tx *gorm.DB, rows []T) error {
	if len(rows) == 0 {
		return nil
	}
	return tx.CreateInBatches(&rows, 500).Error
}

// derive maps snap to the derived rows of run: a fit per fit with its
// estimate, and each model's minimum and optimal guidance.
func derive(run uuid.UUID, snap *catalog.CatalogSnapshot) ([]model.Fit, []model.FitEstimate, []model.Guidance) {
	d := model.Derivation{GeneratorRunID: run}
	fits := make([]model.Fit, len(snap.Fits))
	var estimates []model.FitEstimate
	for i, f := range snap.Fits {
		fits[i] = model.Fit{Base: model.Base{ID: uuid.New()}, Derivation: d, FitKey: catalog.FitKey(f), ModelSlug: f.ModelSlug,
			DeviceFamily: f.DeviceFamily, Profile: f.Profile, Quantization: f.Quantization, KVCacheDType: f.KVCacheDType,
			Mode: string(f.Mode), GPUCount: f.GPUCount, Nodes: f.Nodes, MaxContext: f.MaxContext, VRAMRequiredGB: f.VRAMRequiredGB}
		if p := f.Performance; p != nil {
			estimates = append(estimates, model.FitEstimate{Derivation: d, FitID: fits[i].ID, Source: p.Source,
				DecodeTokensPerSec: p.DecodeTokensPerSec, ConcurrentSequences: p.ConcurrentSequences,
				AggregateTokensPerSec: p.AggregateTokensPerSec, Context: p.Context})
		}
	}
	var guidance []model.Guidance
	for _, m := range snap.Models {
		for _, g := range []struct {
			kind string
			r    *catalog.Recommendation
		}{{"minimum", m.Deployment.Minimum}, {"optimal", m.Deployment.Optimal}} {
			r := g.r
			if r == nil {
				continue
			}
			guidance = append(guidance, model.Guidance{Derivation: d, ModelSlug: m.Slug, Kind: g.kind, Family: r.Family, GPUs: r.GPUs,
				Nodes: r.Nodes, Quantization: r.Quantization, Preset: r.Preset, Context: r.Context, NodeTypes: r.NodeTypes,
				DecodeTokensPerSec: r.DecodeTokensPerSec, AggregateTokensPerSec: r.AggregateTokensPerSec, Source: r.Source,
				Confidence: r.Confidence, FitKeys: r.FitKeys})
		}
	}
	return fits, estimates, guidance
}
