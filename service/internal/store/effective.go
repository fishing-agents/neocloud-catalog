// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

// Effective is the effective catalog: per natural key the local object, else the upstream one,
// ordered as pick orders them (node types by id). Extensions.SnapshotDigest stays empty.
func (s *Store) Effective(ctx context.Context) (Inputs, error) {
	var in Inputs
	err := s.read(ctx, func(tx *gorm.DB) error {
		var err error
		in, err = effective(tx)
		return err
	})
	return in, err
}

// read runs fn on one snapshot of the database.
func (s *Store) read(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return s.db.WithContext(ctx).Transaction(fn, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
}

func effective(tx *gorm.DB) (Inputs, error) {
	up, local, payloads, err := loadEffective(tx)
	if err != nil {
		return Inputs{}, err
	}
	return merge(up, local, payloads)
}

func loadEffective(tx *gorm.DB) (up, local rowSet, payloads []model.ExtensionPayload, err error) {
	if up, err = load(tx, model.LayerUpstream, scope{}); err != nil {
		return
	}
	if local, err = load(tx, model.LayerLocal, scope{}); err != nil {
		return
	}
	err = tx.Where("layer = ?", model.LayerOverlay).Find(&payloads).Error
	return
}

func merge(up, local rowSet, payloads []model.ExtensionPayload) (Inputs, error) {
	u, err := assemble(up)
	if err != nil {
		return Inputs{}, fmt.Errorf("%s layer: %w", model.LayerUpstream, err)
	}
	l, err := assemble(local)
	if err != nil {
		return Inputs{}, fmt.Errorf("%s layer: %w", model.LayerLocal, err)
	}
	nodeTypes := pick(u.nodeTypes, l.nodeTypes)
	slices.SortFunc(nodeTypes, func(a, b catalog.NodeType) int { return strings.Compare(a.ID, b.ID) })
	return Inputs{
		Families:   pick(u.families, l.families),
		NodeTypes:  nodeTypes,
		Models:     pick(u.models, l.models),
		Drafts:     pick(u.drafts, l.drafts),
		Backends:   pick(u.backends, l.backends),
		Extensions: overlay(payloads),
	}, nil
}

// Overlay is the overlay layer's payloads keyed as catalog.Merge expects, SnapshotDigest empty.
func (s *Store) Overlay(ctx context.Context) (catalog.Overlay, error) {
	var payloads []model.ExtensionPayload
	err := s.db.WithContext(ctx).Where("layer = ?", model.LayerOverlay).Find(&payloads).Error
	return overlay(payloads), err
}

func overlay(payloads []model.ExtensionPayload) catalog.Overlay {
	o := catalog.Overlay{SchemaVersion: catalog.SchemaVersion}
	kinds := payloadKinds(&o)
	for _, p := range payloads {
		ids := kinds[slices.IndexFunc(kinds, func(k payloadKind) bool { return k.kind == p.ObjectKind })].ids
		if *ids == nil {
			*ids = map[string]catalog.Extensions{}
		}
		if (*ids)[p.ObjectKey] == nil {
			(*ids)[p.ObjectKey] = catalog.Extensions{}
		}
		(*ids)[p.ObjectKey][p.Namespace] = json.RawMessage(p.Payload)
	}
	return o
}

// entry is one layer's object with its curated position.
type entry[T any] struct {
	pos int
	key string
	obj T
}

// pick takes per key the local entry over the upstream one: upstream keys by (position, key), an
// override in its upstream row's place, then local-only keys by (position, key). Local positions
// are frozen at write time, so they order only local-only keys and upstream reorders carry overrides.
func pick[T any](upstream, local []entry[T]) []T {
	if len(upstream)+len(local) == 0 {
		return nil
	}
	order := func(a, b entry[T]) int { return cmp.Or(cmp.Compare(a.pos, b.pos), strings.Compare(a.key, b.key)) }
	slices.SortFunc(upstream, order)
	slices.SortFunc(local, order)
	overrides := make(map[string]T, len(local))
	for _, e := range local {
		overrides[e.key] = e.obj
	}
	out := make([]T, 0, len(upstream)+len(local))
	for _, e := range upstream {
		if o, ok := overrides[e.key]; ok {
			e.obj = o
			delete(overrides, e.key)
		}
		out = append(out, e.obj)
	}
	for _, e := range local {
		if _, ok := overrides[e.key]; ok {
			out = append(out, e.obj)
		}
	}
	return out
}

// rowSet holds the rows of a layer, or of one object, each table in curated order; files[i] belong to sources[i].
type rowSet struct {
	families  []model.Family
	nodeTypes []model.NodeType
	models    []model.Model
	quants    []model.ModelQuantization
	presets   []model.Preset
	sources   []model.ModelSource
	files     [][]model.ModelFile
	drafts    []model.DraftModel
	backends  []model.Backend
	versions  []model.BackendVersion
	flags     []model.FlagTable
	payloads  []model.ExtensionPayload
}

func (rs rowSet) size() int {
	return len(rs.families) + len(rs.nodeTypes) + len(rs.models) + len(rs.quants) + len(rs.presets) + len(rs.sources) +
		len(rs.drafts) + len(rs.backends) + len(rs.versions) + len(rs.flags) + len(rs.payloads)
}

func load(tx *gorm.DB, layer string, sc scope) (rowSet, error) {
	var rs rowSet
	for _, t := range []struct {
		table, order string
		dest         any
	}{
		{"families", "position, name", &rs.families},
		{"node_types", "node_type_id", &rs.nodeTypes},
		{"models", "position, slug", &rs.models},
		{"model_quantizations", "model_slug, position, name", &rs.quants},
		{"presets", "model_slug, position, name", &rs.presets},
		{"model_sources", "preference, type, repo", &rs.sources},
		{"draft_models", "position, slug", &rs.drafts},
		{"backends", "position, name", &rs.backends},
		{"backend_versions", "backend, position, version", &rs.versions},
		{"flag_tables", "backend, backend_version", &rs.flags},
		{"extension_payloads", "object_kind, object_key, namespace", &rs.payloads},
	} {
		q := tx.Table(t.table).Where("layer = ?", layer)
		if sc.kind != "" {
			col, ok := sc.column(t.table)
			if !ok {
				continue
			}
			q = q.Where(col+" = ?", sc.key)
		}
		if err := q.Order(t.order).Find(t.dest).Error; err != nil {
			return rowSet{}, err
		}
	}
	rs.files = make([][]model.ModelFile, len(rs.sources))
	if len(rs.sources) == 0 {
		return rs, nil
	}
	ids := make([]uuid.UUID, len(rs.sources))
	index := make(map[uuid.UUID]int, len(rs.sources))
	for i, src := range rs.sources {
		ids[i], index[src.ID] = src.ID, i
	}
	var files []model.ModelFile
	if err := tx.Where("model_source_id IN ?", ids).Order("position, path").Find(&files).Error; err != nil {
		return rowSet{}, err
	}
	for _, f := range files {
		i := index[f.ModelSourceID]
		rs.files[i] = append(rs.files[i], f)
	}
	return rs, nil
}

// orEmpty keeps lists the snapshot marshals without omitempty (quantizations, versions) from turning null.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

type objects struct {
	families  []entry[catalog.Family]
	nodeTypes []entry[catalog.NodeType]
	models    []entry[catalog.ModelTemplate]
	drafts    []entry[catalog.DraftModel]
	backends  []entry[catalog.Backend]
}

// assemble builds one layer's core objects from that layer's children; a row without its parent there is an error.
func assemble(rs rowSet) (objects, error) {
	var o objects
	var problems []string
	for _, r := range rs.families {
		o.families = append(o.families, entry[catalog.Family]{r.Position, r.Name, catalog.Family{Name: r.Name, Vendor: r.Vendor,
			Architecture: r.Architecture, ComputeCapability: r.ComputeCapability, DTypes: r.DTypes, MemoryGB: r.MemoryGB,
			MemoryBandwidthGBs: r.MemoryBandwidthGBs, TFLOPS: r.TFLOPS.Data(), Interconnect: r.Interconnect,
			InterconnectGBs: r.InterconnectGBs, Sources: r.Sources}})
	}
	for _, r := range rs.nodeTypes {
		o.nodeTypes = append(o.nodeTypes, entry[catalog.NodeType]{0, r.NodeTypeID,
			catalog.NodeType{ID: r.NodeTypeID, Family: r.Family, GPUs: r.GPUs, Source: r.DocURL}})
	}

	type owner struct{ model, quant, draft string }
	sources := map[owner][]catalog.ModelSource{}
	for i, r := range rs.sources {
		src := catalog.ModelSource{Type: r.Type, Repo: r.Repo, Revision: r.Revision}
		for _, f := range rs.files[i] {
			if f.Layer != r.Layer {
				problems = append(problems, fmt.Sprintf("file %s of %s in layer %s", f.Path, r.Repo, f.Layer))
			}
			src.Files = append(src.Files, catalog.ModelFile{Path: f.Path, Size: f.SizeBytes, Digest: f.Digest})
		}
		k := owner{r.ModelSlug, r.Quantization, r.DraftSlug}
		sources[k] = append(sources[k], src)
	}
	quants := map[string][]catalog.Quantization{}
	for _, r := range rs.quants {
		k := owner{model: r.ModelSlug, quant: r.Name}
		quants[r.ModelSlug] = append(quants[r.ModelSlug], catalog.Quantization{Name: r.Name, Kind: r.Kind,
			RequiresDType: r.RequiresDType, WeightsGB: r.WeightsGB, Sources: sources[k]})
		delete(sources, k)
	}
	presets := map[string][]catalog.Preset{}
	for _, r := range rs.presets {
		presets[r.ModelSlug] = append(presets[r.ModelSlug], catalog.Preset{Name: r.Name, Quantization: r.Quantization,
			Backend: r.Backend, BackendVersion: r.BackendVersion, MinBackendVersion: r.MinBackendVersion, Purpose: r.Purpose,
			Context: r.Context, TensorParallel: r.TensorParallel, KVCacheDType: r.KVCacheDType, PrefixCaching: r.PrefixCaching,
			ToolParser: r.ToolParser, ReasoningParser: r.ReasoningParser, ChatTemplate: r.ChatTemplate,
			Speculative: r.Speculative.Data(), Topology: r.Topology.Data(), Env: r.Env.Data(), ExtraArgs: r.ExtraArgs, Vendors: r.Vendors})
	}
	for _, r := range rs.models {
		o.models = append(o.models, entry[catalog.ModelTemplate]{r.Position, r.Slug, catalog.ModelTemplate{Slug: r.Slug,
			Name: r.Name, Family: r.Family, Description: r.Description, Task: r.Task, Capabilities: r.Capabilities,
			ParamsB: r.ParamsB, ParamsActiveB: r.ParamsActiveB, ReleaseDate: r.ReleaseDate, Home: r.Home, Icon: r.Icon,
			Aliases: r.Aliases, DefaultContext: r.DefaultContext, ContextMax: r.ContextMax, Quantizations: orEmpty(quants[r.Slug]),
			Architecture: r.Architecture.Data(), License: r.License, Gated: r.Gated, Presets: presets[r.Slug]}})
		delete(quants, r.Slug)
		delete(presets, r.Slug)
	}
	for _, r := range rs.drafts {
		d := catalog.DraftModel{Slug: r.Slug, Method: r.Method, Targets: r.Targets, WeightsGB: r.WeightsGB, Backends: r.Backends}
		k := owner{draft: r.Slug}
		switch srcs := sources[k]; len(srcs) {
		case 0:
		case 1:
			d.Source = srcs[0]
		default:
			problems = append(problems, fmt.Sprintf("draft model %s has %d sources", r.Slug, len(srcs)))
		}
		delete(sources, k)
		o.drafts = append(o.drafts, entry[catalog.DraftModel]{r.Position, r.Slug, d})
	}

	flags := map[[2]string]catalog.FlagTable{}
	for _, r := range rs.flags {
		flags[[2]string{r.Backend, r.BackendVersion}] = r.Flags.Data()
	}
	versions := map[string][]catalog.BackendVersion{}
	for _, r := range rs.versions {
		k := [2]string{r.Backend, r.Version}
		f, ok := flags[k]
		if !ok {
			problems = append(problems, fmt.Sprintf("backend version %s/%s has no flag table", r.Backend, r.Version))
		}
		delete(flags, k)
		versions[r.Backend] = append(versions[r.Backend], catalog.BackendVersion{Version: r.Version, Images: r.Images.Data(),
			Command: r.Command, HealthPath: r.HealthPath, MinComputeCapability: r.MinComputeCapability,
			Quantizations: r.Quantizations.Data(), Flags: f, Defaults: r.Defaults.Data()})
	}
	for _, r := range rs.backends {
		o.backends = append(o.backends, entry[catalog.Backend]{r.Position, r.Name, catalog.Backend{Name: r.Name, Versions: orEmpty(versions[r.Name])}})
		delete(versions, r.Name)
	}

	for k := range sources {
		problems = append(problems, fmt.Sprintf("source of %s", strings.Trim(k.model+"/"+k.quant+"/"+k.draft, "/")))
	}
	for slug := range quants {
		problems = append(problems, "quantizations of model "+slug)
	}
	for slug := range presets {
		problems = append(problems, "presets of model "+slug)
	}
	for k := range flags {
		problems = append(problems, "flag table of backend version "+k[0]+"/"+k[1])
	}
	for name := range versions {
		problems = append(problems, "versions of backend "+name)
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return objects{}, fmt.Errorf("inconsistent rows: %s", strings.Join(problems, "; "))
	}
	return o, nil
}
