// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap imports the core repository's data/*.json and data/templates/*.json into the
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

	catalog "github.com/fishman/neocloud-catalog"
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

// Load's version is the sha256 over the files' names and contents. Templates are validated here: the
// generator copies them into the snapshot as they are.
func Load(dir string) (store.Inputs, string, error) {
	var in store.Inputs
	templates, err := filepath.Glob(filepath.Join(dir, "templates", "*.json"))
	if err != nil {
		return store.Inputs{}, "", err
	}
	in.Templates = make([]catalog.Template, len(templates))
	type file struct {
		name string
		v    any
	}
	files := []file{
		{"families.json", &in.Families},
		{"node_types.json", &in.NodeTypes},
		{"models.json", &in.Models},
		{"draft_models.json", &in.Drafts},
		{"backends.json", &in.Backends},
	}
	for i, path := range templates {
		files = append(files, file{filepath.Join("templates", filepath.Base(path)), &in.Templates[i]})
	}
	h := sha256.New()
	for _, f := range files {
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
	if err := catalog.ValidateTemplates(in.Templates); err != nil {
		return store.Inputs{}, "", err
	}
	return in, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
