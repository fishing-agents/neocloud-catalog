// Package release freezes generator runs as releases and publishes them as
// neocloud-catalog's release workflow does and neocloud-api's catalogsrc
// verifies: the public snapshot as a signed single-layer OCI artifact, and the
// adapter exports with a signed SHA256SUMS.
package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/google/uuid"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
	_ "github.com/fishman/neocloud-catalog/adapters/airunway"
	_ "github.com/fishman/neocloud-catalog/adapters/gpustack"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// BundleType is the artifact and layer type of a Sigstore bundle attached to
// a signed manifest as an OCI referrer, as cosign v3 attaches it.
const BundleType = "application/vnd.dev.sigstore.bundle.v0.3+json"

const (
	emptyType       = "application/vnd.oci.empty.v1+json"
	titleAnnotation = "org.opencontainers.image.title"
)

// Release is a release row, bound to the store it was read from.
type Release struct {
	model.Release
	s *store.Store
}

// Create freezes the succeeded generator run generatorRunID as a release: its
// version is the run's generation time, its digest the run's snapshot's at
// that version, and its overlay the overlay layer's payloads now, pinned to
// that digest.
func Create(ctx context.Context, s *store.Store, generatorRunID uuid.UUID) (Release, error) {
	run, err := s.GeneratorRun(ctx, generatorRunID)
	if err != nil {
		return Release{}, err
	}
	if run.Status != model.RunSucceeded || run.Snapshot == nil {
		return Release{}, fmt.Errorf("generator run %s is %s, not succeeded", run.ID, run.Status)
	}
	snap := *run.Snapshot
	if snap.SchemaVersion != catalog.SchemaVersion {
		return Release{}, fmt.Errorf("generator run %s has schema %s; this build releases %s", run.ID, snap.SchemaVersion, catalog.SchemaVersion)
	}
	snap.Version = snap.GeneratedAt.UTC().Format(catalog.VersionLayout)
	overlay, err := s.Overlay(ctx)
	if err != nil {
		return Release{}, err
	}
	overlay.SnapshotDigest = snap.Digest()
	if _, err := catalog.Merge(snap, overlay); err != nil {
		return Release{}, fmt.Errorf("generator run %s: %w", run.ID, err)
	}
	rel := model.Release{Version: snap.Version, Digest: overlay.SnapshotDigest, SchemaVersion: snap.SchemaVersion,
		GeneratorRunID: run.ID, Overlay: overlay}
	if err := s.CreateRelease(ctx, &rel); err != nil {
		return Release{}, fmt.Errorf("release %s: %w", rel.Version, err)
	}
	return Release{rel, s}, nil
}

// Get returns the release of version.
func Get(ctx context.Context, s *store.Store, version string) (Release, error) {
	rel, err := s.Release(ctx, version)
	return Release{rel, s}, err
}

// Snapshot returns the release's public snapshot: its generator run's at the
// release version, refused once it no longer has the release digest.
func (r Release) Snapshot(ctx context.Context) (catalog.CatalogSnapshot, error) {
	run, err := r.s.GeneratorRun(ctx, r.GeneratorRunID)
	if err != nil {
		return catalog.CatalogSnapshot{}, err
	}
	if run.Snapshot == nil {
		return catalog.CatalogSnapshot{}, fmt.Errorf("release %s: generator run %s holds no snapshot", r.Version, run.ID)
	}
	snap := *run.Snapshot
	snap.Version = r.Version
	if d := snap.Digest(); d != r.Digest {
		return catalog.CatalogSnapshot{}, fmt.Errorf("release %s: generator run %s now has digest %s, not the released %s", r.Version, run.ID, d, r.Digest)
	}
	return snap, nil
}

// Target is where Publish pushes a release: Ref names the repository and the
// tag clients poll (e.g. ghcr.io/org/neocloud-catalog:v4); Signer signs the
// pushed manifest.
type Target struct {
	Ref    string
	Signer Signer
}

// Publish pushes the release's snapshot to t.Ref's repository as the catalog
// artifact, one catalog.ArtifactType layer under the version annotation, signs
// its manifest, then points t.Ref's tag at it and records the release as
// published there. A published release is not pushed again: publishing it
// again changes nothing when its content still makes the published manifest,
// and is refused otherwise. A release older than a published one is refused,
// as clients refuse a catalog that is not newer than theirs.
func Publish(ctx context.Context, rel Release, t Target) error {
	if t.Signer == nil {
		return errors.New("publish: no signer")
	}
	ref, err := name.ParseReference(t.Ref)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	cur, err := Get(ctx, rel.s, rel.Version)
	if err != nil {
		return err
	}
	snap, err := cur.Snapshot(ctx)
	if err != nil {
		return err
	}
	body, err := snap.Encode()
	if err != nil {
		return err
	}
	a, err := newArtifact(catalog.ArtifactType, "catalog.json", body, map[string]string{catalog.VersionAnnotation: cur.Version}, nil)
	if err != nil {
		return err
	}
	repo := ref.Context()
	at := repo.Digest(a.desc.Digest.String()).String()
	if cur.PublishedAt != nil {
		if cur.ArtifactRef != at {
			return fmt.Errorf("release %s is published as %s, not %s", cur.Version, cur.ArtifactRef, at)
		}
		return nil
	}
	newer, err := cur.s.NewerPublished(ctx, cur.Version)
	if err != nil {
		return err
	}
	if newer != "" {
		return fmt.Errorf("release %s is older than the published release %s", cur.Version, newer)
	}
	opts := remoteOptions(ctx)
	if err := a.push(repo, opts); err != nil {
		return fmt.Errorf("push %s: %w", at, err)
	}
	if err := t.Signer.SignManifest(ctx, repo, a.desc); err != nil {
		return fmt.Errorf("sign %s: %w", at, err)
	}
	if tag, ok := ref.(name.Tag); ok {
		if err := remote.Put(tag, a, opts...); err != nil {
			return fmt.Errorf("tag %s: %w", tag, err)
		}
	}
	_, err = cur.s.PublishRelease(ctx, cur.ID, at)
	return err
}

// WriteExports writes the release's adapter exports to dir, the assets the
// file-based release workflow publishes: every format of every registered
// adapter, SHA256SUMS over them, and signer's Sigstore bundle over SHA256SUMS
// as SHA256SUMS.sigstore.json.
func WriteExports(ctx context.Context, rel Release, dir string, signer Signer) error {
	snap, err := rel.Snapshot(ctx)
	if err != nil {
		return err
	}
	x := adapters.Exporter{Extensions: adapters.ExportExtensions()}
	if err := x.Write(dir, snap); err != nil {
		return fmt.Errorf("release %s exports: %w", rel.Version, err)
	}
	sums := filepath.Join(dir, "SHA256SUMS")
	return signer.SignBlob(ctx, sums, sums+".sigstore.json")
}

func remoteOptions(ctx context.Context) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)}
}

// artifact is a single-layer OCI artifact: an empty config, one layer of the
// artifact type, and the manifest, which a subject makes a referrer. Its
// manifest digest depends on its content only.
type artifact struct {
	config, layer v1.Layer
	manifest      []byte
	desc          v1.Descriptor
}

// newArtifact builds the artifact of body; a title names the layer's file,
// as oras push records it.
func newArtifact(artifactType, title string, body []byte, annotations map[string]string, subject *v1.Descriptor) (artifact, error) {
	a := artifact{config: static.NewLayer([]byte("{}"), emptyType), layer: static.NewLayer(body, types.MediaType(artifactType))}
	config, err := partial.Descriptor(a.config)
	if err != nil {
		return a, err
	}
	layer, err := partial.Descriptor(a.layer)
	if err != nil {
		return a, err
	}
	if title != "" {
		layer.Annotations = map[string]string{titleAnnotation: title}
	}
	a.manifest, err = json.Marshal(v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, ArtifactType: artifactType,
		Config: *config, Layers: []v1.Descriptor{*layer}, Annotations: annotations, Subject: subject})
	if err != nil {
		return a, err
	}
	d, size, err := v1.SHA256(bytes.NewReader(a.manifest))
	a.desc = v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: d, Size: size, ArtifactType: artifactType}
	return a, err
}

func (a artifact) RawManifest() ([]byte, error) { return a.manifest, nil }

func (a artifact) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }

// push writes the artifact's blobs and its manifest, by digest, to repo.
func (a artifact) push(repo name.Repository, opts []remote.Option) error {
	for _, l := range []v1.Layer{a.config, a.layer} {
		if err := remote.WriteLayer(repo, l, opts...); err != nil {
			return err
		}
	}
	return remote.Put(repo.Digest(a.desc.Digest.String()), a, opts...)
}
