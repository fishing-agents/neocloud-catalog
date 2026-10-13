// SPDX-License-Identifier: Apache-2.0

// Package upstream imports an upstream service's signed snapshot into the upstream
// layer and its pinned overlay into the overlay layer, in one transaction.
// A release older than the last imported one is refused, so a replay cannot
// roll back.
package upstream

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

type Importer struct{ release, overlay *Source }

// New returns the importer of release and overlay; overlay may be nil.
func New(release, overlay *Source) *Importer { return &Importer{release, overlay} }

// Name is also its runs' source: an import, never curation.
func (*Importer) Name() string { return "upstream" }

func (i *Importer) Origin() importers.Origin {
	return importers.Origin{Layer: model.LayerUpstream, Source: i.Name(), SourceURL: i.release.Ref()}
}

func (i *Importer) Run(ctx context.Context, s *store.Store, run uuid.UUID) (importers.Summary, error) {
	rel, err := i.release.Check(ctx)
	if err != nil {
		return importers.Summary{}, err
	}
	// An early exit; ReplaceUpstream decides under the write lock.
	if newest, by, err := s.ImportedVersion(ctx, i.Name()); err != nil {
		return importers.Summary{}, err
	} else if rel.Time.Before(newest) {
		return importers.Summary{}, fmt.Errorf("upstream version %s is older than %s, imported by run %s",
			rel.Version, newest.UTC().Format(catalog.VersionLayout), by)
	}
	snap, err := i.release.Pull(ctx, rel)
	if err != nil {
		return importers.Summary{}, err
	}
	if major(snap.SchemaVersion) != major(catalog.SchemaVersion) {
		return importers.Summary{}, fmt.Errorf("snapshot schema %s, want %s.x", snap.SchemaVersion, major(catalog.SchemaVersion))
	}
	in := inputs(snap)
	sum := importers.Summary{InputVersion: rel.Version + " " + snap.Digest(), Sources: &importers.SourceCounts{Read: 1}}
	seeded, overlayURL := snap, ""
	if i.overlay != nil {
		ov, err := i.overlay.Check(ctx)
		if err != nil {
			return importers.Summary{}, fmt.Errorf("overlay %w", err)
		}
		o, err := i.overlay.PullOverlay(ctx, ov)
		if err != nil {
			return importers.Summary{}, err
		}
		if seeded, err = catalog.Merge(snap, o); err != nil {
			return importers.Summary{}, err
		}
		in.Extensions, overlayURL = o, i.overlay.Ref()
		sum.InputVersion += " overlay " + ov.Digest.String()
		sum.Sources.Read++
	}
	if err := seeded.Validate(); err != nil {
		return importers.Summary{}, fmt.Errorf("snapshot: %w", err)
	}
	sum.Changes, sum.Conflicts, err = s.ReplaceUpstream(ctx, run, rel.Time, in, overlayURL)
	return sum, err
}

// inputs are snap's curated objects, without generated fallback node types or
// derived model fields.
func inputs(snap catalog.CatalogSnapshot) store.Inputs {
	in := store.Inputs{Families: snap.Families, Models: snap.Models, Drafts: snap.DraftModels, Backends: snap.Backends}
	for _, nt := range snap.NodeTypes {
		if nt != (catalog.NodeType{ID: catalog.FallbackNodeType(nt.Family, nt.GPUs), Family: nt.Family, GPUs: nt.GPUs}) {
			in.NodeTypes = append(in.NodeTypes, nt)
		}
	}
	return in
}

func major(version string) string {
	m, _, _ := strings.Cut(version, ".")
	return m
}
