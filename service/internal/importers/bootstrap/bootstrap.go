// Package bootstrap imports the core repository's data/*.json into the
// upstream layer as an import, not curation: later importers replace its values.
package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

type Importer struct {
	dir, sourceURL string
}

func New(dir string) (*Importer, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Importer{dir: dir, sourceURL: (&url.URL{Scheme: "file", Path: abs}).String()}, nil
}

func (*Importer) Name() string { return "bootstrap-from-json" }

func (i *Importer) Origin() importers.Origin {
	return importers.Origin{Layer: model.LayerUpstream, Source: i.Name(), SourceURL: i.sourceURL}
}

func (i *Importer) Run(ctx context.Context, s *store.Store, run uuid.UUID) (importers.Summary, error) {
	in, version, err := Load(i.dir)
	if err != nil {
		return importers.Summary{}, err
	}
	changes, conflicts, err := s.ReplaceLayer(ctx, model.LayerUpstream, run, in)
	return importers.Summary{InputVersion: version, Changes: changes, Conflicts: conflicts}, err
}

// Load's version is the sha256 over the files' names and contents.
func Load(dir string) (store.Inputs, string, error) {
	var in store.Inputs
	h := sha256.New()
	for _, f := range []struct {
		name string
		v    any
	}{
		{"families.json", &in.Families},
		{"node_types.json", &in.NodeTypes},
		{"models.json", &in.Models},
		{"draft_models.json", &in.Drafts},
		{"backends.json", &in.Backends},
	} {
		b, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			return store.Inputs{}, "", err
		}
		if err := json.Unmarshal(b, f.v); err != nil {
			return store.Inputs{}, "", fmt.Errorf("%s: %w", f.name, err)
		}
		fmt.Fprintf(h, "%s %d\n", f.name, len(b))
		h.Write(b)
	}
	return in, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
