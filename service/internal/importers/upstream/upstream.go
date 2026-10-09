// Package upstream imports an upstream catalog service's release, as an
// operator deployment runs on top of ours: the signed public snapshot under
// upstream.ref replaces the upstream layer with its curated objects and, when
// upstream.overlay_ref is set, the signed overlay pinned to that snapshot
// replaces the overlay layer, in one transaction; without one the overlay
// layer is emptied, as it mirrors what the import verified. The local layer
// stays.
//
// Verification is neocloud-api's catalog sync's (Source mirrors its
// catalogsrc): a signature by the configured identity or key on the manifest
// digest, the layer checked against the signed manifest, a version not older
// than any release an upstream run wrote (the version is signed, so a
// replayed older release cannot roll the layers back; checked again under the
// store's write lock, where the run records its version), the schema major,
// the overlay merged with catalog.Merge (its pin to the snapshot digest, its
// ids, its namespaces) and the merged snapshot validated. Any failure fails
// the run and changes neither layer.
//
// The input version is "<release version> <snapshot digest>", plus
// " overlay <overlay manifest digest>" with an overlay.
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

// Importer imports the release of an upstream Source and, when it has one,
// the overlay of another.
type Importer struct{ release, overlay *Source }

// New returns the importer of release and of overlay, which may be nil.
func New(release, overlay *Source) *Importer { return &Importer{release, overlay} }

// Name is also its runs' source: an import, never curation (R21).
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

// inputs are the curated objects of snap, which Generate reproduces it from:
// its node types without the generated fallbacks. The derived model fields
// are not stored.
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
