package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

// Patch is what an importer read about objects of its run's layer: values of
// their value columns and the file manifests of their sources. CurationURL is
// where the importer read the values it marks curated.
type Patch struct {
	Values      []Values
	Manifests   []Manifest
	CurationURL string
}

// Values are imported values by column name of the object Kind (KindFamily,
// KindModel, KindQuantization or KindDraft) keyed Key. Curated names the
// columns whose value the importer read from curation rather than its source.
type Values struct {
	Kind, Key string
	Columns   map[string]any
	Curated   []string
}

// Manifest is the files of a source read at Revision; the source belongs to a
// quantization (ModelSlug, Quantization) or a draft model (DraftSlug).
type Manifest struct {
	ModelSlug, Quantization, DraftSlug string
	Type, Repo, Revision               string
	Files                              []catalog.ModelFile
}

// Conflict is an imported value a curated one was kept over (R12). Field is a
// column, or files/<path> of a curated manifest, where a file one side lacks
// is null.
type Conflict struct {
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Field    string `json:"field"`
	Curated  any    `json:"curated"`
	Imported any    `json:"imported"`
}

// Patch writes p into the layer of the running import run run, in one
// transaction, and touches no other row or column: a value sets one column of
// an existing row, whose row provenance stays its origin; values of a family
// the layer lacks give all its columns and add it, last, as the run's row; a
// manifest replaces the files of its source, which must still be pinned at
// the manifest's revision. As in ReplaceLayer an unchanged value keeps its
// provenance and an importer run keeps a curated value, unless the value is
// curated too; a source with curated files keeps them all. It returns the
// changes and the kept curated values that differ from the imported ones.
func (s *Store) Patch(ctx context.Context, run uuid.UUID, p Patch) (Changes, []Conflict, error) {
	changes := Changes{}
	count := func(table string, c Change) {
		if c != (Change{}) {
			t := changes[table]
			changes[table] = Change{t.Inserted + c.Inserted, t.Updated + c.Updated, t.Deleted + c.Deleted}
		}
	}
	var conflicts []Conflict
	err := s.write(ctx, func(tx *gorm.DB) error {
		var r model.ImportRun
		if err := tx.Take(&r, "id = ?", run).Error; err != nil {
			return fmt.Errorf("import run %s: %w", run, err)
		}
		if r.Status != model.RunRunning || r.Layer == model.LayerOverlay {
			return fmt.Errorf("import run %s is %s in layer %s, not running in a core layer", run, r.Status, r.Layer)
		}
		st := stamp{layer: r.Layer, source: r.Source, sourceURL: r.SourceURL, asOf: r.StartedAt, run: &r.ID}
		for _, v := range p.Values {
			vs := st
			vs.curated, vs.curationURL = v.Curated, p.CurationURL
			var c Change
			var kept []Conflict
			var err error
			switch v.Kind {
			case KindFamily:
				c, kept, err = patchRow(tx, vs, v, addFamily, "name = ?", v.Key)
			case KindModel:
				c, kept, err = patchRow[model.Model](tx, vs, v, nil, "slug = ?", v.Key)
			case KindQuantization:
				slug, name, _ := strings.Cut(v.Key, "/")
				c, kept, err = patchRow[model.ModelQuantization](tx, vs, v, nil, "model_slug = ? AND name = ?", slug, name)
			case KindDraft:
				c, kept, err = patchRow[model.DraftModel](tx, vs, v, nil, "slug = ?", v.Key)
			default:
				err = fmt.Errorf("%w: cannot patch a %s", ErrInvalid, v.Kind)
			}
			if err != nil {
				return err
			}
			count(provenanceKinds[v.Kind].table, c)
			conflicts = append(conflicts, kept...)
		}
		for _, m := range p.Manifests {
			c, kept, err := patchFiles(tx, st, m)
			if err != nil {
				return err
			}
			count("model_files", c)
			conflicts = append(conflicts, kept...)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return changes, conflicts, nil
}

// addFamily makes f the new family key, last.
func addFamily(tx *gorm.DB, f *model.Family, key string) error {
	f.Name = key
	return tx.Raw(`SELECT COALESCE(MAX(position) + 1, 0) FROM families`).Scan(&f.Position).Error
}

// patchRow sets the columns of v on the row of st's layer that where selects.
// Without one, add (nil: none) prepares a new row of v's key.
func patchRow[T any](tx *gorm.DB, st stamp, v Values, add func(*gorm.DB, *T, string) error, where string, args ...any) (Change, []Conflict, error) {
	var o T
	found := tx.Where("layer = ?", st.layer).Where(where, args...).Limit(1).Find(&o)
	if found.Error != nil {
		return Change{}, nil, found.Error
	}
	isNew := found.RowsAffected == 0
	if isNew && add == nil {
		return Change{}, nil, fmt.Errorf("%s %s %q: %w", st.layer, v.Kind, v.Key, ErrNotFound)
	}
	cols, err := valueColumns(tx, &o)
	if err != nil {
		return Change{}, nil, err
	}
	w, old := o, &o
	if isNew {
		if len(v.Columns) != len(cols) {
			return Change{}, nil, fmt.Errorf("%w: new %s %q: %d of its %d value columns given", ErrInvalid, v.Kind, v.Key, len(v.Columns), len(cols))
		}
		if err := add(tx, &w, v.Key); err != nil {
			return Change{}, nil, err
		}
		old = nil
	}
	wv := reflect.ValueOf(&w).Elem()
	names := slices.Sorted(maps.Keys(v.Columns))
	fields, imported := make([]reflect.Value, len(names)), make([]any, len(names))
	for i, name := range names {
		c := slices.IndexFunc(cols, func(c column) bool { return c.db == name })
		if c < 0 {
			return Change{}, nil, fmt.Errorf("%w: %s %q: no value column %s", ErrInvalid, v.Kind, v.Key, name)
		}
		f, x := wv.FieldByName(cols[c].name), reflect.ValueOf(v.Columns[name])
		if !x.IsValid() || !x.Type().AssignableTo(f.Type()) {
			// A fresh value: json.Unmarshal merges into the map or struct the
			// row holds.
			n := reflect.New(f.Type())
			if b, err := json.Marshal(v.Columns[name]); !x.IsValid() || err != nil || json.Unmarshal(b, n.Interface()) != nil {
				return Change{}, nil, fmt.Errorf("%w: %s %q: %s takes no %T", ErrInvalid, v.Kind, v.Key, name, v.Columns[name])
			}
			x = n.Elem()
		}
		f.Set(x)
		fields[i], imported[i] = f, f.Interface()
	}
	fp, err := fieldProvenance(tx, st, &w, old)
	if err != nil {
		return Change{}, nil, err
	}
	wm := meta(&w)
	if isNew {
		*wm.prov = model.Provenance{Layer: st.layer, Source: st.source, SourceURL: st.sourceURL, AsOf: st.asOf, ImportRunID: st.run}
		*wm.fields = datatypes.NewJSONType(fp)
		return Change{Inserted: 1}, nil, tx.Create(&w).Error
	}
	// fieldProvenance stamps a column without provenance even when its value
	// is unchanged; a patch stamps only the columns it sets.
	prev := meta(&o).fields.Data()
	for col := range fp {
		if _, set := v.Columns[col]; set {
			continue
		}
		if p, ok := prev[col]; ok {
			fp[col] = p
		} else {
			delete(fp, col)
		}
	}
	var kept []Conflict
	for i, f := range fields {
		if got := f.Interface(); !reflect.DeepEqual(got, imported[i]) {
			kept = append(kept, Conflict{Kind: v.Kind, Key: v.Key, Field: names[i], Curated: got, Imported: imported[i]})
		}
	}
	if reflect.DeepEqual(fp, wm.fields.Data()) && sameContent(&w, &o) {
		return Change{}, kept, nil
	}
	*wm.fields = datatypes.NewJSONType(fp)
	return Change{Updated: 1}, kept, tx.Save(&w).Error
}

// patchFiles replaces the files of m's source in st's layer, unless the
// source's files are curated and st is an importer's.
func patchFiles(tx *gorm.DB, st stamp, m Manifest) (Change, []Conflict, error) {
	key := strings.Trim(m.ModelSlug+"/"+m.Quantization+"/"+m.DraftSlug, "/") + " " + m.Type + " " + m.Repo
	var src model.ModelSource
	err := tx.Where("layer = ? AND model_slug = ? AND quantization = ? AND draft_slug = ? AND type = ? AND repo = ?",
		st.layer, m.ModelSlug, m.Quantization, m.DraftSlug, m.Type, m.Repo).Take(&src).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return Change{}, nil, fmt.Errorf("%s source %s: %w", st.layer, key, ErrNotFound)
	case err != nil:
		return Change{}, nil, err
	case src.Revision != m.Revision:
		return Change{}, nil, fmt.Errorf("%w: source %s is pinned at %s, read at %s", ErrConflict, key, src.Revision, m.Revision)
	}
	want := make([]model.ModelFile, len(m.Files))
	imported := make(map[string]catalog.ModelFile, len(m.Files))
	for i, f := range m.Files {
		if _, dup := imported[f.Path]; dup || f.Path == "" {
			return Change{}, nil, fmt.Errorf("%w: source %s: file %d %q: empty or duplicate path", ErrInvalid, key, i, f.Path)
		}
		imported[f.Path] = f
		want[i] = model.ModelFile{ModelSourceID: src.ID, Path: f.Path, Position: i, SizeBytes: f.Size, Digest: f.Digest}
	}
	var have []model.ModelFile
	if err := tx.Where("model_source_id = ?", src.ID).Order("position, path").Find(&have).Error; err != nil {
		return Change{}, nil, err
	}
	if st.source == model.SourceCuration || !slices.ContainsFunc(have, func(f model.ModelFile) bool { return f.Source == model.SourceCuration }) {
		c, err := syncTable(tx, st, have, want, func(r *model.ModelFile) string { return r.Path })
		return c, nil, err
	}
	var kept []Conflict
	conflict := func(path string, curated, imp *catalog.ModelFile) {
		kept = append(kept, Conflict{Kind: "source", Key: key, Field: "files/" + path, Curated: curated, Imported: imp})
	}
	for _, h := range have {
		c := catalog.ModelFile{Path: h.Path, Size: h.SizeBytes, Digest: h.Digest}
		if f, ok := imported[h.Path]; !ok {
			conflict(h.Path, &c, nil)
		} else if f != c {
			conflict(h.Path, &c, &f)
		}
		delete(imported, h.Path)
	}
	for _, f := range m.Files {
		if _, extra := imported[f.Path]; extra {
			conflict(f.Path, nil, &f)
		}
	}
	return Change{}, kept, nil
}
