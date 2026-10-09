// Package release freezes generator runs as releases and publishes them as the
// release workflow does: a signed single-layer OCI artifact of the snapshot,
// and adapter exports with a signed SHA256SUMS.
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

// BundleType is the media type of a Sigstore bundle attached as an OCI referrer, as cosign v3 does.
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

// Create freezes the succeeded generator run as a release versioned by its
// generation time, with the current overlay pinned to its digest.
func Create(ctx context.Context, s *store.Store, generatorRunID uuid.UUID) (Release, error) {
	run, err := s.GeneratorRun(ctx, generatorRunID)
	if err != nil {
		return Release{}, err
	}
	if run.Status != model.RunSucceeded || run.Snapshot == nil {
		return Release{}, fmt.Errorf("%w: generator run %s is %s, not succeeded", store.ErrConflict, run.ID, run.Status)
	}
	snap := *run.Snapshot
	if snap.SchemaVersion != catalog.SchemaVersion {
		return Release{}, fmt.Errorf("%w: generator run %s has schema %s; this build releases %s", store.ErrConflict, run.ID, snap.SchemaVersion, catalog.SchemaVersion)
	}
	snap.Version = snap.GeneratedAt.UTC().Format(catalog.VersionLayout)
	overlay, err := s.Overlay(ctx)
	if err != nil {
		return Release{}, err
	}
	overlay.SnapshotDigest = snap.Digest()
	if _, err := catalog.Merge(snap, overlay); err != nil {
		return Release{}, fmt.Errorf("%w: generator run %s: %w", store.ErrConflict, run.ID, err)
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

// Snapshot returns the release's public snapshot, refused once it no longer
// has the release digest.
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

// Target is where Publish pushes: Ref is the polled repository:tag, OverlayRef
// the optional overlay's, Signer signs every manifest. PlainHTTP talks HTTP to
// a registry without TLS.
type Target struct {
	Ref, OverlayRef string
	Signer          Signer
	PlainHTTP       bool
}

// Publish pushes and signs the snapshot artifact (and with t.OverlayRef the
// overlay), then moves the tags and records where it was published.
// Republishing identical content is a no-op; differing content, an overlay
// published elsewhere, or a release older than a published one is refused,
// since clients refuse a catalog not newer than theirs.
func Publish(ctx context.Context, rel Release, t Target) error {
	if t.Signer == nil {
		return errors.New("publish: no signer")
	}
	var nameOpts []name.Option
	if t.PlainHTTP {
		nameOpts = append(nameOpts, name.Insecure)
	}
	ref, err := name.ParseReference(t.Ref, nameOpts...)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	var overlayRef name.Reference
	if t.OverlayRef != "" {
		if overlayRef, err = name.ParseReference(t.OverlayRef, nameOpts...); err != nil {
			return fmt.Errorf("publish overlay: %w", err)
		}
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
	annotations := map[string]string{catalog.VersionAnnotation: cur.Version}
	a, err := newArtifact(catalog.ArtifactType, "catalog.json", body, annotations, nil)
	if err != nil {
		return err
	}
	at := ref.Context().Digest(a.desc.Digest.String()).String()
	pushes, overlayAt := []push{{ref, a}}, ""
	if overlayRef != nil {
		body, err := json.Marshal(cur.Overlay)
		if err != nil {
			return err
		}
		o, err := newArtifact(catalog.OverlayArtifactType, "overlay.json", body, annotations, nil)
		if err != nil {
			return err
		}
		pushes = append(pushes, push{overlayRef, o})
		overlayAt = overlayRef.Context().Digest(o.desc.Digest.String()).String()
	}
	if cur.PublishedAt != nil {
		switch {
		case cur.ArtifactRef != at:
			return fmt.Errorf("%w: release %s is published as %s, not %s", store.ErrConflict, cur.Version, cur.ArtifactRef, at)
		case overlayAt == "" || cur.OverlayRef == overlayAt:
			return nil
		case cur.OverlayRef != "":
			return fmt.Errorf("%w: release %s's overlay is published as %s, not %s", store.ErrConflict, cur.Version, cur.OverlayRef, overlayAt)
		}
		pushes = pushes[1:]
	}
	newer, err := cur.s.NewerPublished(ctx, cur.Version)
	if err != nil {
		return err
	}
	if newer != "" {
		return fmt.Errorf("%w: release %s is older than the published release %s", store.ErrConflict, cur.Version, newer)
	}
	if err := publish(ctx, t.Signer, pushes); err != nil {
		return err
	}
	if cur.PublishedAt != nil {
		return cur.s.PublishOverlay(ctx, cur.ID, overlayAt)
	}
	_, err = cur.s.PublishRelease(ctx, cur.ID, at, overlayAt)
	return err
}

// push is an artifact and the reference it is published under.
type push struct {
	ref name.Reference
	a   artifact
}

// publish pushes and signs every artifact, then points the tags at them.
func publish(ctx context.Context, signer Signer, pushes []push) error {
	opts := remoteOptions(ctx)
	for _, p := range pushes {
		repo := p.ref.Context()
		d := repo.Digest(p.a.desc.Digest.String())
		if err := p.a.push(repo, opts); err != nil {
			return fmt.Errorf("push %s: %w", d, err)
		}
		if err := signer.SignManifest(ctx, repo, p.a.desc); err != nil {
			return fmt.Errorf("sign %s: %w", d, err)
		}
	}
	for _, p := range pushes {
		if tag, ok := p.ref.(name.Tag); ok {
			if err := remote.Put(tag, p.a, opts...); err != nil {
				return fmt.Errorf("tag %s: %w", tag, err)
			}
		}
	}
	return nil
}

// WriteExports writes every adapter's exports to dir, SHA256SUMS over them, and
// signer's bundle over that as SHA256SUMS.sigstore.json.
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

// artifact is a single-layer OCI artifact; a subject makes it a referrer. Its
// manifest digest depends on its content only.
type artifact struct {
	config, layer v1.Layer
	manifest      []byte
	desc          v1.Descriptor
}

// newArtifact builds the artifact of body; title names the layer's file, as oras push does.
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

// push writes the artifact's blobs and manifest, by digest, to repo.
func (a artifact) push(repo name.Repository, opts []remote.Option) error {
	for _, l := range []v1.Layer{a.config, a.layer} {
		if err := remote.WriteLayer(repo, l, opts...); err != nil {
			return err
		}
	}
	return remote.Put(repo.Digest(a.desc.Digest.String()), a, opts...)
}
