// Package store reads and writes the catalog's curated layers. Per natural key
// a local object overrides the upstream one as a whole: a model comes with the
// quantizations, sources, files and presets of its own layer only, a draft
// model with its source and files, a backend with its versions and flag
// tables; layers never mix inside one object. The overlay layer holds
// extension payloads only. Every write takes one lock, so writers serialize.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

// ErrNotFound reports an object no layer holds.
var ErrNotFound = errors.New("not found")

// ErrInvalid reports objects a write refuses; ErrConflict a change the
// current state refuses.
var (
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

// Object kinds of Delete and Provenance, keyed by name, id or slug. A
// quantization is keyed "<model slug>/<name>"; only Provenance takes it, as it
// is curated with its model.
const (
	KindFamily       = "family"
	KindNodeType     = "node_type"
	KindModel        = "model"
	KindQuantization = "quantization"
	KindDraft        = "draft_model"
	KindBackend      = "backend"
)

// Store is the curated catalog in Postgres.
type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db: db} }

// Inputs are the curated objects catalog.Generate reads plus the overlay's
// extension payloads. The derived model fields KVBytesPerToken and Deployment
// are not stored: Generate recomputes them.
type Inputs struct {
	Families   []catalog.Family
	NodeTypes  []catalog.NodeType
	Models     []catalog.ModelTemplate
	Drafts     []catalog.DraftModel
	Backends   []catalog.Backend
	Extensions catalog.Overlay
}

// ProvenanceEntry is where one layer's row of an object came from. Effective
// marks the row the effective catalog uses; Fields is the provenance per
// column of the mixed-source tables (nil for backends).
type ProvenanceEntry struct {
	Layer       string
	Source      string
	SourceURL   string
	AsOf        time.Time
	ImportRunID *uuid.UUID
	Effective   bool
	Fields      model.FieldProvenance
}

type tableKey struct{ table, key string }

// objectTables lists, per curated object kind, the tables of its rows and the
// column holding the object's key, its own table first. Files follow their
// source.
var objectTables = map[string][]tableKey{
	KindFamily:   {{"families", "name"}},
	KindNodeType: {{"node_types", "node_type_id"}},
	KindModel:    {{"models", "slug"}, {"model_quantizations", "model_slug"}, {"presets", "model_slug"}, {"model_sources", "model_slug"}},
	KindDraft:    {{"draft_models", "slug"}, {"model_sources", "draft_slug"}},
	KindBackend:  {{"backends", "name"}, {"backend_versions", "backend"}, {"flag_tables", "backend"}},
}

// scope is one object of a layer, or the whole layer when kind is empty.
type scope struct{ kind, key string }

func (sc scope) column(table string) (string, bool) {
	for _, t := range objectTables[sc.kind] {
		if t.table == table {
			return t.key, true
		}
	}
	return "", false
}

// stamp is the provenance a write gives the rows it changes; the value
// columns curated take curation from curationURL instead.
type stamp struct {
	layer, source, sourceURL string
	asOf                     time.Time
	run                      *uuid.UUID
	curated                  []string
	curationURL              string
}

// of is the stamp of the value column col.
func (st stamp) of(col string) stamp {
	if slices.Contains(st.curated, col) {
		st.source, st.sourceURL = model.SourceCuration, st.curationURL
	}
	return st
}

func (st stamp) field() model.FieldSource {
	return model.FieldSource{Source: st.source, SourceURL: st.sourceURL, AsOf: st.asOf, ImportRunID: st.run}
}

// Change counts the rows a write inserted, updated and deleted in one table.
type Change struct {
	Inserted int `json:"inserted,omitempty"`
	Updated  int `json:"updated,omitempty"`
	Deleted  int `json:"deleted,omitempty"`
}

// Changes are a write's changes by table; a table it left alone is absent.
type Changes map[string]Change

// ReplaceLayer makes layer hold exactly in, in one transaction, written by the
// running import run of that layer; positions follow input order. A row whose
// content is unchanged keeps its provenance, so re-running the same input
// changes nothing. A changed row takes the run's source, source URL and start
// time; per column too, except that an unchanged value keeps its provenance
// and an importer run (source other than curation) keeps a curated value
// (R12). The overlay layer takes only in.Extensions, whose ids must exist in
// the effective catalog; the other layers take no payloads.
func (s *Store) ReplaceLayer(ctx context.Context, layer string, run uuid.UUID, in Inputs) (Changes, error) {
	want, err := toRows(layer, in)
	if err != nil {
		return nil, err
	}
	var changes Changes
	err = s.write(ctx, func(tx *gorm.DB) error {
		var r model.ImportRun
		if err := tx.Take(&r, "id = ?", run).Error; err != nil {
			return fmt.Errorf("import run %s: %w", run, err)
		}
		if r.Layer != layer || r.Status != model.RunRunning {
			return fmt.Errorf("import run %s is %s in layer %s, not running in layer %s", run, r.Status, r.Layer, layer)
		}
		if layer == model.LayerOverlay {
			if err := checkPayloadIDs(tx, in.Extensions); err != nil {
				return err
			}
		}
		have, err := load(tx, layer, scope{})
		if err != nil {
			return err
		}
		changes, err = apply(tx, stamp{layer: layer, source: r.Source, sourceURL: r.SourceURL, asOf: r.StartedAt, run: &r.ID}, have, want)
		return err
	})
	if err != nil {
		return nil, err
	}
	return changes, nil
}

// Upsert curates object in the local layer, replacing the local object of its
// key with its children. object is a catalog.Family, NodeType, ModelTemplate,
// DraftModel or Backend. It keeps the position of its key's local or upstream
// row; a new key goes last.
func (s *Store) Upsert(ctx context.Context, object any) error {
	return s.write(ctx, func(tx *gorm.DB) error { return upsert(tx, object) })
}

// upsert is Upsert in tx.
func upsert(tx *gorm.DB, object any) error {
	var in Inputs
	var sc scope
	switch o := object.(type) {
	case catalog.Family:
		in.Families, sc = []catalog.Family{o}, scope{KindFamily, o.Name}
	case catalog.NodeType:
		in.NodeTypes, sc = []catalog.NodeType{o}, scope{KindNodeType, o.ID}
	case catalog.ModelTemplate:
		in.Models, sc = []catalog.ModelTemplate{o}, scope{KindModel, o.Slug}
	case catalog.DraftModel:
		in.Drafts, sc = []catalog.DraftModel{o}, scope{KindDraft, o.Slug}
	case catalog.Backend:
		in.Backends, sc = []catalog.Backend{o}, scope{KindBackend, o.Name}
	default:
		return fmt.Errorf("store: cannot upsert a %T", object)
	}
	want, err := toRows(model.LayerLocal, in)
	if err != nil {
		return err
	}
	var pos *int
	switch sc.kind {
	case KindFamily:
		pos = &want.families[0].Position
	case KindModel:
		pos = &want.models[0].Position
	case KindDraft:
		pos = &want.drafts[0].Position
	case KindBackend:
		pos = &want.backends[0].Position
	}
	if pos != nil {
		t := objectTables[sc.kind][0]
		err := tx.Raw(`SELECT COALESCE((SELECT position FROM `+t.table+` WHERE `+t.key+` = ? ORDER BY layer LIMIT 1), `+
			`(SELECT MAX(position) + 1 FROM `+t.table+`), 0)`, sc.key).Scan(pos).Error
		if err != nil {
			return err
		}
	}
	have, err := load(tx, model.LayerLocal, sc)
	if err != nil {
		return err
	}
	_, err = apply(tx, stamp{layer: model.LayerLocal, source: model.SourceCuration, asOf: time.Now()}, have, want)
	return err
}

// Delete removes the local object kind/key with its children, which restores
// the upstream object of that key if there is one.
func (s *Store) Delete(ctx context.Context, kind, key string) error {
	if _, ok := objectTables[kind]; !ok {
		return fmt.Errorf("store: cannot delete a %s", kind)
	}
	return s.write(ctx, func(tx *gorm.DB) error {
		have, err := load(tx, model.LayerLocal, scope{kind, key})
		if err != nil {
			return err
		}
		if have.size() == 0 {
			return fmt.Errorf("local %s %q: %w", kind, key, ErrNotFound)
		}
		_, err = apply(tx, stamp{}, have, rowSet{})
		return err
	})
}

// provenanceKinds are the tables Provenance reads per kind, with the key
// columns a "/"-separated key splits into and whether field provenance is kept.
var provenanceKinds = map[string]struct {
	table  string
	keys   []string
	fields bool
}{
	KindFamily:       {"families", []string{"name"}, true},
	KindNodeType:     {"node_types", []string{"node_type_id"}, true},
	KindModel:        {"models", []string{"slug"}, true},
	KindQuantization: {"model_quantizations", []string{"model_slug", "name"}, true},
	KindDraft:        {"draft_models", []string{"slug"}, true},
	KindBackend:      {"backends", []string{"name"}, false},
}

type provenanceRow struct {
	model.Provenance
	ObjectKey       string
	Local           bool
	FieldProvenance datatypes.JSONType[model.FieldProvenance]
}

// Provenance lists the local and upstream rows of an object, local first. A
// quantization's effective row is the one in its effective model's layer.
func (s *Store) Provenance(ctx context.Context, kind, key string) ([]ProvenanceEntry, error) {
	if _, ok := provenanceKinds[kind]; !ok {
		return nil, fmt.Errorf("store: no provenance of a %s", kind)
	}
	var byKey map[string][]ProvenanceEntry
	err := s.read(ctx, func(tx *gorm.DB) error {
		var err error
		byKey, err = provenance(tx, kind, key)
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(byKey[key]) == 0 {
		return nil, fmt.Errorf("%s %q: %w", kind, key, ErrNotFound)
	}
	return byKey[key], nil
}

// Provenances are the provenance entries of objects by kind and key.
type Provenances map[string]map[string][]ProvenanceEntry

// EffectiveProvenance is Effective plus the provenance of every object, read
// on one snapshot.
func (s *Store) EffectiveProvenance(ctx context.Context) (Inputs, Provenances, error) {
	var in Inputs
	all := Provenances{}
	err := s.read(ctx, func(tx *gorm.DB) error {
		var err error
		if in, err = effective(tx); err != nil {
			return err
		}
		for kind := range provenanceKinds {
			if all[kind], err = provenance(tx, kind, ""); err != nil {
				return err
			}
		}
		return nil
	})
	return in, all, err
}

// provenance reads the local and upstream rows of kind, of the object key or
// of every object when key is "", by key, local first.
func provenance(tx *gorm.DB, kind, key string) (map[string][]ProvenanceEntry, error) {
	k := provenanceKinds[kind]
	top := objectTables[kind]
	if kind == KindQuantization {
		top = objectTables[KindModel]
	}
	cols := "t." + strings.Join(k.keys, " || '/' || t.") + " AS object_key, t.layer, t.source, t.source_url, t.as_of, t.import_run_id, " +
		"EXISTS (SELECT 1 FROM " + top[0].table + " o WHERE o." + top[0].key + " = t." + k.keys[0] + " AND o.layer = 'local') AS local"
	if k.fields {
		cols += ", t.field_provenance"
	}
	q := tx.Table(k.table+" t").Select(cols).Where("t.layer IN ?", []string{model.LayerLocal, model.LayerUpstream})
	if key != "" {
		parts := strings.SplitN(key, "/", len(k.keys))
		if len(parts) != len(k.keys) {
			return nil, nil
		}
		for i, c := range k.keys {
			q = q.Where("t."+c+" = ?", parts[i])
		}
	}
	var rows []provenanceRow
	if err := q.Order("object_key, t.layer").Find(&rows).Error; err != nil {
		return nil, err
	}
	byKey := map[string][]ProvenanceEntry{}
	for _, r := range rows {
		effective := r.Layer == model.LayerUpstream
		if r.Local {
			effective = r.Layer == model.LayerLocal
		}
		byKey[r.ObjectKey] = append(byKey[r.ObjectKey], ProvenanceEntry{Layer: r.Layer, Source: r.Source, SourceURL: r.SourceURL,
			AsOf: r.AsOf, ImportRunID: r.ImportRunID, Effective: effective, Fields: r.FieldProvenance.Data()})
	}
	return byKey, nil
}

// write runs fn in a transaction holding the catalog's write lock.
func (s *Store) write(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('neocloud-catalog-store'))`).Error; err != nil {
			return err
		}
		return fn(tx)
	})
}

// toRows maps in to rows of layer with positions in input order. It refuses
// empty and duplicate keys, payloads outside the overlay layer (including
// inside objects) and core objects in it.
func toRows(layer string, in Inputs) (rowSet, error) {
	var rs rowSet
	var errs []error
	seen := map[string]bool{}
	key := func(kind string, parts ...string) {
		k := kind + " " + strings.Join(parts, "/")
		switch {
		case slices.Contains(parts, ""):
			errs = append(errs, fmt.Errorf("%s: empty key", k))
		case seen[k]:
			errs = append(errs, fmt.Errorf("%s: duplicate", k))
		}
		seen[k] = true
	}
	noPayloads := func(kind, id string, e catalog.Extensions) {
		if len(e) > 0 {
			errs = append(errs, fmt.Errorf("%s %s: extension payloads belong to the overlay layer", kind, id))
		}
	}
	source := func(src model.ModelSource, files []catalog.ModelFile, owner string) {
		key("source", owner, src.Type, src.Repo)
		rows := make([]model.ModelFile, len(files))
		for i, f := range files {
			key("file", owner, src.Type, src.Repo, f.Path)
			rows[i] = model.ModelFile{Path: f.Path, Position: i, SizeBytes: f.Size, Digest: f.Digest}
		}
		rs.sources = append(rs.sources, src)
		rs.files = append(rs.files, rows)
	}

	for i, f := range in.Families {
		key(KindFamily, f.Name)
		noPayloads(KindFamily, f.Name, f.Extensions)
		rs.families = append(rs.families, model.Family{Name: f.Name, Position: i, Vendor: f.Vendor, Architecture: f.Architecture,
			ComputeCapability: f.ComputeCapability, DTypes: f.DTypes, MemoryGB: f.MemoryGB, MemoryBandwidthGBs: f.MemoryBandwidthGBs,
			TFLOPS: datatypes.NewJSONType(f.TFLOPS), Interconnect: f.Interconnect, InterconnectGBs: f.InterconnectGBs, Sources: f.Sources})
	}
	for _, nt := range in.NodeTypes {
		key(KindNodeType, nt.ID)
		rs.nodeTypes = append(rs.nodeTypes, model.NodeType{NodeTypeID: nt.ID, Family: nt.Family, GPUs: nt.GPUs, DocURL: nt.Source})
	}
	for i, m := range in.Models {
		key(KindModel, m.Slug)
		noPayloads(KindModel, m.Slug, m.Extensions)
		rs.models = append(rs.models, model.Model{Slug: m.Slug, Position: i, Name: m.Name, Family: m.Family, Task: m.Task,
			Capabilities: m.Capabilities, Description: m.Description, ParamsB: m.ParamsB, ParamsActiveB: m.ParamsActiveB,
			ReleaseDate: m.ReleaseDate, Home: m.Home, Icon: m.Icon, Aliases: m.Aliases, License: m.License, Gated: m.Gated,
			Architecture: datatypes.NewJSONType(m.Architecture), DefaultContext: m.DefaultContext, ContextMax: m.ContextMax})
		for j, q := range m.Quantizations {
			key(KindQuantization, m.Slug, q.Name)
			rs.quants = append(rs.quants, model.ModelQuantization{ModelSlug: m.Slug, Name: q.Name, Position: j, Kind: q.Kind,
				RequiresDType: q.RequiresDType, WeightsGB: q.WeightsGB})
			for k, src := range q.Sources {
				source(model.ModelSource{ModelSlug: m.Slug, Quantization: q.Name, Type: src.Type, Repo: src.Repo,
					Revision: src.Revision, Preference: k}, src.Files, m.Slug+"/"+q.Name)
			}
		}
		for j, p := range m.Presets {
			key("preset", m.Slug, p.Name)
			noPayloads("preset", m.Slug+"/"+p.Name, p.Extensions)
			rs.presets = append(rs.presets, model.Preset{ModelSlug: m.Slug, Name: p.Name, Position: j, Quantization: p.Quantization,
				Backend: p.Backend, BackendVersion: p.BackendVersion, MinBackendVersion: p.MinBackendVersion, Purpose: p.Purpose,
				Context: p.Context, TensorParallel: p.TensorParallel, KVCacheDType: p.KVCacheDType, PrefixCaching: p.PrefixCaching,
				ToolParser: p.ToolParser, ReasoningParser: p.ReasoningParser, ChatTemplate: p.ChatTemplate,
				Speculative: datatypes.NewJSONType(p.Speculative), Topology: datatypes.NewJSONType(p.Topology),
				Env: datatypes.NewJSONType(p.Env), ExtraArgs: p.ExtraArgs, Vendors: p.Vendors})
		}
	}
	for i, d := range in.Drafts {
		key(KindDraft, d.Slug)
		rs.drafts = append(rs.drafts, model.DraftModel{Slug: d.Slug, Position: i, Method: d.Method, Targets: d.Targets,
			WeightsGB: d.WeightsGB, Backends: d.Backends})
		if src := d.Source; src.Type != "" || src.Repo != "" || src.Revision != "" || src.Files != nil {
			source(model.ModelSource{DraftSlug: d.Slug, Type: src.Type, Repo: src.Repo, Revision: src.Revision}, src.Files, d.Slug)
		}
	}
	for i, b := range in.Backends {
		key(KindBackend, b.Name)
		rs.backends = append(rs.backends, model.Backend{Name: b.Name, Position: i})
		for j, v := range b.Versions {
			key("backend version", b.Name, v.Version)
			noPayloads("backend version", b.Name+"/"+v.Version, v.Extensions)
			rs.versions = append(rs.versions, model.BackendVersion{Backend: b.Name, Version: v.Version, Position: j,
				Images: datatypes.NewJSONType(v.Images), Command: v.Command, HealthPath: v.HealthPath,
				MinComputeCapability: v.MinComputeCapability, Quantizations: datatypes.NewJSONType(v.Quantizations),
				Defaults: datatypes.NewJSONType(v.Defaults)})
			rs.flags = append(rs.flags, model.FlagTable{Backend: b.Name, BackendVersion: v.Version, Flags: datatypes.NewJSONType(v.Flags)})
		}
	}
	for _, k := range payloadKinds(&in.Extensions) {
		for id, exts := range *k.ids {
			for ns, p := range exts {
				key("extension", k.kind, id, ns)
				rs.payloads = append(rs.payloads, model.ExtensionPayload{ObjectKind: k.kind, ObjectKey: id, Namespace: ns, Payload: datatypes.JSON(p)})
			}
		}
	}

	switch core := len(rs.families) + len(rs.nodeTypes) + len(rs.models) + len(rs.drafts) + len(rs.backends); {
	case layer == model.LayerOverlay && core > 0:
		errs = append(errs, errors.New("the overlay layer holds extension payloads only"))
	case layer != model.LayerOverlay && len(rs.payloads) > 0:
		errs = append(errs, fmt.Errorf("extension payloads belong to the overlay layer, not %s", layer))
	}
	if len(errs) > 0 {
		return rs, fmt.Errorf("%w: %w", ErrInvalid, errors.Join(errs...))
	}
	return rs, nil
}

// checkPayloadIDs refuses payloads of objects the effective catalog lacks. Fits
// are derived, so a fit id is checked as far as curation decides it: its
// family and its model's quantization; catalog.Merge checks the rest.
func checkPayloadIDs(tx *gorm.DB, o catalog.Overlay) error {
	eff, err := effective(tx)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, f := range eff.Families {
		known["families "+f.Name] = true
	}
	for _, m := range eff.Models {
		known["models "+m.Slug] = true
		for _, p := range m.Presets {
			known["presets "+m.Slug+"/"+p.Name] = true
		}
		for _, q := range m.Quantizations {
			known["quantization "+m.Slug+"/"+q.Name] = true
		}
	}
	for _, b := range eff.Backends {
		for _, v := range b.Versions {
			known["backends "+b.Name+"/"+v.Version] = true
		}
	}
	var errs []error
	for _, k := range payloadKinds(&o) {
		for id := range *k.ids {
			ok := known[k.kind+" "+id]
			if k.kind == "fits" {
				p := strings.Split(id, "/")
				ok = (len(p) == 5 || len(p) == 6 && strings.HasSuffix(p[5], "-node")) &&
					known["families "+p[1]] && known["quantization "+p[0]+"/"+p[3]]
			}
			if !ok {
				errs = append(errs, fmt.Errorf("extension payloads of unknown %s %q", k.kind, id))
			}
		}
	}
	return errors.Join(errs...)
}

type payloadKind struct {
	kind string
	ids  *map[string]catalog.Extensions
}

// payloadKinds pairs each extension_payloads object kind with its map in o.
func payloadKinds(o *catalog.Overlay) []payloadKind {
	return []payloadKind{{"families", &o.Families}, {"models", &o.Models}, {"presets", &o.Presets}, {"fits", &o.Fits}, {"backends", &o.Backends}}
}

func keyOf(parts ...string) string { return strings.Join(parts, "\x00") }

// apply turns the rows have into want table by table (see syncTable) and
// returns what it changed. Files follow their source by its id.
func apply(tx *gorm.DB, st stamp, have, want rowSet) (Changes, error) {
	files := func() (Change, error) {
		var old, fresh []model.ModelFile
		for _, fs := range have.files {
			old = append(old, fs...)
		}
		for i, fs := range want.files {
			for _, f := range fs {
				f.ModelSourceID = want.sources[i].ID
				fresh = append(fresh, f)
			}
		}
		return syncTable(tx, st, old, fresh, func(r *model.ModelFile) string { return keyOf(r.ModelSourceID.String(), r.Path) })
	}
	changes := Changes{}
	for _, step := range []struct {
		table string
		sync  func() (Change, error)
	}{
		{"families", func() (Change, error) {
			return syncTable(tx, st, have.families, want.families, func(r *model.Family) string { return r.Name })
		}},
		{"node_types", func() (Change, error) {
			return syncTable(tx, st, have.nodeTypes, want.nodeTypes, func(r *model.NodeType) string { return r.NodeTypeID })
		}},
		{"models", func() (Change, error) {
			return syncTable(tx, st, have.models, want.models, func(r *model.Model) string { return r.Slug })
		}},
		{"model_quantizations", func() (Change, error) {
			return syncTable(tx, st, have.quants, want.quants, func(r *model.ModelQuantization) string { return keyOf(r.ModelSlug, r.Name) })
		}},
		{"presets", func() (Change, error) {
			return syncTable(tx, st, have.presets, want.presets, func(r *model.Preset) string { return keyOf(r.ModelSlug, r.Name) })
		}},
		{"model_sources", func() (Change, error) {
			return syncTable(tx, st, have.sources, want.sources, func(r *model.ModelSource) string {
				return keyOf(r.ModelSlug, r.Quantization, r.DraftSlug, r.Type, r.Repo)
			})
		}},
		{"model_files", files},
		{"draft_models", func() (Change, error) {
			return syncTable(tx, st, have.drafts, want.drafts, func(r *model.DraftModel) string { return r.Slug })
		}},
		{"backends", func() (Change, error) {
			return syncTable(tx, st, have.backends, want.backends, func(r *model.Backend) string { return r.Name })
		}},
		{"backend_versions", func() (Change, error) {
			return syncTable(tx, st, have.versions, want.versions, func(r *model.BackendVersion) string { return keyOf(r.Backend, r.Version) })
		}},
		{"flag_tables", func() (Change, error) {
			return syncTable(tx, st, have.flags, want.flags, func(r *model.FlagTable) string { return keyOf(r.Backend, r.BackendVersion) })
		}},
		{"extension_payloads", func() (Change, error) {
			return syncTable(tx, st, have.payloads, want.payloads, func(r *model.ExtensionPayload) string {
				return keyOf(r.ObjectKind, r.ObjectKey, r.Namespace)
			})
		}},
	} {
		c, err := step.sync()
		if err != nil {
			return nil, err
		}
		if c != (Change{}) {
			changes[step.table] = c
		}
	}
	return changes, nil
}

// syncTable turns the rows have of one table into want by key: an unchanged
// row stays as it is, a changed one is updated in place with st's provenance,
// a new one is inserted and a row want lacks is deleted. Every want row ends
// with its id.
func syncTable[T any](tx *gorm.DB, st stamp, have, want []T, key func(*T) string) (Change, error) {
	old := make(map[string]*T, len(have))
	for i := range have {
		old[key(&have[i])] = &have[i]
	}
	var c Change
	var fresh []int
	for i := range want {
		w := &want[i]
		o := old[key(w)]
		delete(old, key(w))
		changed, err := stampRow(tx, st, w, o)
		switch {
		case err != nil:
			return c, err
		case !changed:
			*w = *o
		case o == nil:
			fresh = append(fresh, i)
		default:
			if err := tx.Save(w).Error; err != nil {
				return c, err
			}
			c.Updated++
		}
	}
	if len(old) > 0 {
		ids := make([]uuid.UUID, 0, len(old))
		for _, o := range old {
			ids = append(ids, meta(o).base.ID)
		}
		if err := tx.Where("id IN ?", ids).Delete(new(T)).Error; err != nil {
			return c, err
		}
		c.Deleted = len(ids)
	}
	if len(fresh) == 0 {
		return c, nil
	}
	rows := make([]T, len(fresh))
	for j, i := range fresh {
		rows[j] = want[i]
	}
	if err := tx.CreateInBatches(&rows, 500).Error; err != nil {
		return c, err
	}
	for j, i := range fresh {
		want[i] = rows[j]
	}
	c.Inserted = len(rows)
	return c, nil
}

// rowMeta points at the identity, audit and provenance parts of a curated
// row; fields is nil for tables without field provenance.
type rowMeta struct {
	base   *model.Base
	ts     *model.Timestamps
	prov   *model.Provenance
	fields *datatypes.JSONType[model.FieldProvenance]
}

func meta(row any) rowMeta {
	v := reflect.ValueOf(row).Elem()
	m := rowMeta{
		base: v.FieldByName("Base").Addr().Interface().(*model.Base),
		ts:   v.FieldByName("Timestamps").Addr().Interface().(*model.Timestamps),
		prov: v.FieldByName("Provenance").Addr().Interface().(*model.Provenance),
	}
	if f := v.FieldByName("FieldProvenance"); f.IsValid() {
		m.fields = f.Addr().Interface().(*datatypes.JSONType[model.FieldProvenance])
	}
	return m
}

// stampRow gives the new row w the identity of the layer's row o of its key
// (nil for a new key) and st's provenance, settles its field provenance, and
// reports whether w differs from o.
func stampRow[T any](tx *gorm.DB, st stamp, w, o *T) (bool, error) {
	wm := meta(w)
	*wm.prov = model.Provenance{Layer: st.layer, Source: st.source, SourceURL: st.sourceURL, AsOf: st.asOf, ImportRunID: st.run}
	if o == nil {
		if wm.fields != nil {
			fp, err := fieldProvenance(tx, st, w, nil)
			*wm.fields = datatypes.NewJSONType(fp)
			return true, err
		}
		return true, nil
	}
	om := meta(o)
	wm.base.ID, wm.ts.CreatedAt = om.base.ID, om.ts.CreatedAt
	changed := false
	if wm.fields != nil {
		fp, err := fieldProvenance(tx, st, w, o)
		if err != nil {
			return false, err
		}
		*wm.fields = datatypes.NewJSONType(fp)
		changed = !reflect.DeepEqual(fp, om.fields.Data())
	}
	return changed || !sameContent(w, o), nil
}

// fieldProvenance is the provenance per value column of w: a value equal to
// the one in o keeps its provenance, a curated value of o survives an
// importer run's value that is not curated (and is copied back into w), any
// other value is st's.
func fieldProvenance[T any](tx *gorm.DB, st stamp, w, o *T) (model.FieldProvenance, error) {
	cols, err := valueColumns(tx, w)
	if err != nil {
		return nil, err
	}
	var prev model.FieldProvenance
	var ov reflect.Value
	if o != nil {
		prev, ov = meta(o).fields.Data(), reflect.ValueOf(o).Elem()
	}
	wv := reflect.ValueOf(w).Elem()
	fp := make(model.FieldProvenance, len(cols))
	for _, c := range cols {
		p, had := prev[c.db]
		cs := st.of(c.db)
		switch {
		case had && reflect.DeepEqual(wv.FieldByName(c.name).Interface(), ov.FieldByName(c.name).Interface()):
			fp[c.db] = p
		case had && p.Source == model.SourceCuration && cs.source != model.SourceCuration:
			wv.FieldByName(c.name).Set(ov.FieldByName(c.name))
			fp[c.db] = p
		default:
			fp[c.db] = cs.field()
		}
	}
	return fp, nil
}

type column struct{ name, db string }

var schemas, columns sync.Map

// valueColumns are the columns of a mixed-source table that field provenance
// covers: all but identity, audit, provenance, position and the natural key.
func valueColumns(tx *gorm.DB, row any) ([]column, error) {
	t := reflect.TypeOf(row)
	if cols, ok := columns.Load(t); ok {
		return cols.([]column), nil
	}
	s, err := schema.Parse(row, &schemas, tx.NamingStrategy)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{"id": true, "created_at": true, "updated_at": true, "layer": true, "source": true, "source_url": true,
		"as_of": true, "import_run_id": true, "position": true, "field_provenance": true}
	natural := s.LookIndex("idx_" + s.Table + "_natural")
	if natural == nil {
		return nil, fmt.Errorf("%s has no natural key", s.Table)
	}
	for _, f := range natural.Fields {
		skip[f.DBName] = true
	}
	var cols []column
	for _, db := range s.DBNames {
		if !skip[db] {
			cols = append(cols, column{s.FieldsByDBName[db].Name, db})
		}
	}
	columns.Store(t, cols)
	return cols, nil
}

// sameContent compares the curated fields of two rows of one table: all but
// identity, audit, provenance and associations. Payloads compare as JSON
// values, since jsonb keeps neither their key order nor their spacing.
func sameContent(a, b any) bool {
	va, vb := reflect.ValueOf(a).Elem(), reflect.ValueOf(b).Elem()
	for i := range va.NumField() {
		f := va.Type().Field(i)
		if f.Anonymous || f.Name == "FieldProvenance" || f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Struct {
			continue
		}
		x, y := va.Field(i).Interface(), vb.Field(i).Interface()
		if p, ok := x.(datatypes.JSON); ok {
			var px, py any
			if json.Unmarshal(p, &px) != nil || json.Unmarshal(y.(datatypes.JSON), &py) != nil || !reflect.DeepEqual(px, py) {
				return false
			}
			continue
		}
		if !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}
