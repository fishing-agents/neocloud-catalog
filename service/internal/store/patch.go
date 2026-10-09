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
// their value columns and the file manifests of their sources.
type Patch struct {
	Values    []Values
	Manifests []Manifest
}

// Values are imported values by column name of the object Kind (KindModel,
// KindQuantization or KindDraft) keyed Key.
type Values struct {
	Kind, Key string
	Columns   map[string]any
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
// an existing row, whose row provenance stays its origin; a manifest replaces
// the files of its source, which must still be pinned at the manifest's
// revision. As in ReplaceLayer an unchanged value keeps its provenance and an
// importer run keeps a curated value; a source with curated files keeps them
// all. It returns the changes and the kept curated values that differ from
// the imported ones.
func (s *Store) Patch(ctx context.Context, run uuid.UUID, p Patch) (Changes, []Conflict, error) {
	changes := Changes{}
	var conflicts []Conflict
	err := s.write(ctx, func(tx *gorm.DB) error {
		var r model.ImportRun
		if err := tx.Take(&r, "id = ?", run).Error; err != nil {
			return fmt.Errorf("import run %s: %w", run, err)
		}
		if r.Status != model.RunRunning || r.Layer == model.LayerOverlay {
			return fmt.Errorf("import run %s is %s in layer %s, not running in a core layer", run, r.Status, r.Layer)
		}
		st := stamp{r.Layer, r.Source, r.SourceURL, r.StartedAt, &r.ID}
		for _, v := range p.Values {
			var changed bool
			var kept []Conflict
			var err error
			switch v.Kind {
			case KindModel:
				changed, kept, err = patchRow[model.Model](tx, st, v, "slug = ?", v.Key)
			case KindQuantization:
				slug, name, _ := strings.Cut(v.Key, "/")
				changed, kept, err = patchRow[model.ModelQuantization](tx, st, v, "model_slug = ? AND name = ?", slug, name)
			case KindDraft:
				changed, kept, err = patchRow[model.DraftModel](tx, st, v, "slug = ?", v.Key)
			default:
				err = fmt.Errorf("%w: cannot patch a %s", ErrInvalid, v.Kind)
			}
			if err != nil {
				return err
			}
			if changed {
				table := provenanceKinds[v.Kind].table
				c := changes[table]
				c.Updated++
				changes[table] = c
			}
			conflicts = append(conflicts, kept...)
		}
		for _, m := range p.Manifests {
			c, kept, err := patchFiles(tx, st, m)
			if err != nil {
				return err
			}
			if c != (Change{}) {
				total := changes["model_files"]
				changes["model_files"] = Change{total.Inserted + c.Inserted, total.Updated + c.Updated, total.Deleted + c.Deleted}
			}
			conflicts = append(conflicts, kept...)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return changes, conflicts, nil
}

// patchRow sets the columns of v on the row of st's layer that where selects.
func patchRow[T any](tx *gorm.DB, st stamp, v Values, where string, args ...any) (bool, []Conflict, error) {
	var o T
	if err := tx.Where("layer = ?", st.layer).Where(where, args...).Take(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = ErrNotFound
		}
		return false, nil, fmt.Errorf("%s %s %q: %w", st.layer, v.Kind, v.Key, err)
	}
	cols, err := valueColumns(tx, &o)
	if err != nil {
		return false, nil, err
	}
	w := o
	wv := reflect.ValueOf(&w).Elem()
	names := slices.Sorted(maps.Keys(v.Columns))
	fields, imported := make([]reflect.Value, len(names)), make([]any, len(names))
	for i, name := range names {
		c := slices.IndexFunc(cols, func(c column) bool { return c.db == name })
		if c < 0 {
			return false, nil, fmt.Errorf("%w: %s %q: no value column %s", ErrInvalid, v.Kind, v.Key, name)
		}
		f, x := wv.FieldByName(cols[c].name), reflect.ValueOf(v.Columns[name])
		if x.IsValid() && x.Type().AssignableTo(f.Type()) {
			f.Set(x)
		} else if b, err := json.Marshal(v.Columns[name]); !x.IsValid() || err != nil || json.Unmarshal(b, f.Addr().Interface()) != nil {
			return false, nil, fmt.Errorf("%w: %s %q: %s takes no %T", ErrInvalid, v.Kind, v.Key, name, v.Columns[name])
		}
		fields[i], imported[i] = f, f.Interface()
	}
	fp, err := fieldProvenance(tx, st, &w, &o)
	if err != nil {
		return false, nil, err
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
	wm := meta(&w)
	if reflect.DeepEqual(fp, wm.fields.Data()) && sameContent(&w, &o) {
		return false, kept, nil
	}
	*wm.fields = datatypes.NewJSONType(fp)
	return true, kept, tx.Save(&w).Error
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
