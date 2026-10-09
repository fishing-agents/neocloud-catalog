package release_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/elliptic"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/generator"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/release"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const coreDir = "../../.."

var ctx = context.Background()

// generated is a store with the committed data/*.json imported and a succeeded generator run.
func generated(t *testing.T, suffix string) (*store.Store, *gorm.DB, model.GeneratorRun) {
	t.Helper()
	sqlDB, db := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	s := store.New(db)
	imp, err := bootstrap.New(filepath.Join(coreDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importers.Execute(ctx, s, imp, uuid.New()); err != nil {
		t.Fatal(err)
	}
	run, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return s, db, run
}

func create(t *testing.T, s *store.Store, run model.GeneratorRun) release.Release {
	t.Helper()
	rel, err := release.Create(ctx, s, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func registryHost(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// keySigner returns a signer of a fresh password-encrypted cosign.key and its public key.
func keySigner(t *testing.T) (release.Signer, []byte) {
	t.Helper()
	password := []byte("test password")
	priv, pub, err := cryptoutils.GeneratePEMEncodedECDSAKeyPair(elliptic.P256(), cryptoutils.StaticPasswordFunc(password))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := release.NewKeySigner(priv, password)
	if err != nil {
		t.Fatal(err)
	}
	return signer, pub
}

// keyVerifier verifies bundles as catalogsrc trusts a cosign key: no
// transparency log, no timestamps.
func keyVerifier(t *testing.T, pubPEM []byte) *verify.Verifier {
	t.Helper()
	pub, err := cryptoutils.UnmarshalPEMToPublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	sv, err := signature.LoadVerifier(pub, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	tm := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) {
		return root.NewExpiringKey(sv, time.Time{}, time.Time{}), nil
	})
	v, err := verify.NewVerifier(tm, verify.WithNoObserverTimestamps())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// check mirrors catalogsrc Check and Pull: the signature verifies with the key,
// the manifest is one versioned catalog layer, and the layer is that snapshot.
func check(t *testing.T, ref string, pubPEM []byte) (string, []byte, error) {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Get(r)
	if err != nil {
		return "", nil, fmt.Errorf("manifest: %w", err)
	}
	if err := verifyManifest(t, r.Context(), desc.Digest, pubPEM); err != nil {
		return "", nil, err
	}
	var m v1.Manifest
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return "", nil, err
	}
	if m.ArtifactType != catalog.ArtifactType || len(m.Layers) != 1 || string(m.Layers[0].MediaType) != catalog.ArtifactType {
		return "", nil, fmt.Errorf("manifest: want one %s layer in an artifact of that type", catalog.ArtifactType)
	}
	version := m.Annotations[catalog.VersionAnnotation]
	if _, err := catalog.ParseVersion(version); err != nil {
		return "", nil, err
	}
	raw := blob(t, r.Context(), m.Layers[0])
	var snap catalog.CatalogSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return "", nil, err
	}
	if snap.Version != version {
		return "", nil, fmt.Errorf("snapshot version %s does not match the signed version %s", snap.Version, version)
	}
	return version, raw, nil
}

// verifyManifest accepts digest when an attached bundle verifies with the key.
func verifyManifest(t *testing.T, repo name.Repository, digest v1.Hash, pubPEM []byte) error {
	t.Helper()
	want, err := hex.DecodeString(digest.Hex)
	if err != nil {
		t.Fatal(err)
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest(digest.Algorithm, want), verify.WithKey())
	v := keyVerifier(t, pubPEM)
	var errs []error
	for _, d := range bundles(t, repo.Digest(digest.String())) {
		desc, err := remote.Get(repo.Digest(d.Digest.String()))
		if err != nil {
			t.Fatal(err)
		}
		var m v1.Manifest
		if err := json.Unmarshal(desc.Manifest, &m); err != nil {
			t.Fatal(err)
		}
		if len(m.Layers) != 1 || m.Layers[0].MediaType != release.BundleType {
			errs = append(errs, fmt.Errorf("bundle %s: want one %s layer", d.Digest, release.BundleType))
			continue
		}
		var b bundle.Bundle
		if err := b.UnmarshalJSON(blob(t, repo, m.Layers[0])); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := v.Verify(&b, policy); err != nil {
			errs = append(errs, err)
			continue
		}
		return nil
	}
	if len(errs) == 0 {
		return errors.New("signature: the manifest is not signed")
	}
	return fmt.Errorf("signature: %w", errors.Join(errs...))
}

func bundles(t *testing.T, subject name.Digest) []v1.Descriptor {
	t.Helper()
	idx, err := remote.Referrers(subject, remote.WithFilter("artifactType", release.BundleType))
	if err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	var out []v1.Descriptor
	for _, d := range im.Manifests {
		if d.ArtifactType == release.BundleType {
			out = append(out, d)
		}
	}
	return out
}

// blob downloads d from repo and checks its digest.
func blob(t *testing.T, repo name.Repository, d v1.Descriptor) []byte {
	t.Helper()
	l, err := remote.Layer(repo.Digest(d.Digest.String()))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := l.Compressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if h, _, _ := v1.SHA256(bytes.NewReader(raw)); h != d.Digest || int64(len(raw)) != d.Size {
		t.Fatalf("blob %s: content digest %s, %d bytes", d.Digest, h, len(raw))
	}
	return raw
}

func manifestDigest(t *testing.T, ref string) v1.Hash {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Head(r)
	if err != nil {
		t.Fatal(err)
	}
	return desc.Digest
}

// The database refuses to change a release, or to change or delete it once
// published. Republishing identical content is a no-op; differing content or
// another ref is refused.
func TestReleaseImmutable(t *testing.T) {
	s, db, run := generated(t, "release_immutable")
	rel := create(t, s, run)
	want := *run.Snapshot
	want.Version = run.Snapshot.GeneratedAt.UTC().Format(catalog.VersionLayout)
	if rel.Version != want.Version || rel.Digest != want.Digest() || rel.SchemaVersion != catalog.SchemaVersion ||
		rel.GeneratorRunID != run.ID || rel.PublishedAt != nil || rel.ArtifactRef != "" {
		t.Errorf("release = %+v, want version %s, digest %s, schema %s, run %s, unpublished",
			rel.Release, want.Version, want.Digest(), catalog.SchemaVersion, run.ID)
	}
	if _, err := release.Create(ctx, s, run.ID); err == nil {
		t.Error("a generator run was released twice")
	}
	if err := db.Exec(`UPDATE releases SET digest = 'other' WHERE id = ?`, rel.ID).Error; err == nil {
		t.Error("the database accepted a changed release digest")
	}

	signer, _ := keySigner(t)
	host := registryHost(t)
	target := release.Target{Ref: host + "/neocloud/catalog:v4", Signer: signer}
	if err := release.Publish(ctx, rel, target); err != nil {
		t.Fatal(err)
	}
	published, err := release.Get(ctx, s, rel.Version)
	if err != nil {
		t.Fatal(err)
	}
	tagged := manifestDigest(t, target.Ref)
	if published.PublishedAt == nil || published.ArtifactRef != host+"/neocloud/catalog@"+tagged.String() {
		t.Fatalf("published release at %v as %q, want the manifest the tag names, %s", published.PublishedAt, published.ArtifactRef, tagged)
	}
	subject, err := name.NewDigest(published.ArtifactRef)
	if err != nil {
		t.Fatal(err)
	}
	signed := len(bundles(t, subject))

	if err := release.Publish(ctx, published, target); err != nil {
		t.Errorf("publishing identical content again: %v", err)
	}
	again, err := release.Get(ctx, s, rel.Version)
	if err != nil {
		t.Fatal(err)
	}
	if !again.PublishedAt.Equal(*published.PublishedAt) || again.ArtifactRef != published.ArtifactRef || len(bundles(t, subject)) != signed {
		t.Errorf("publishing identical content again changed the release or its signatures")
	}

	if err := release.Publish(ctx, rel, release.Target{Ref: host + "/elsewhere/catalog:v4", Signer: signer}); err == nil {
		t.Error("a published release was published again elsewhere")
	}
	if err := db.Exec(`UPDATE generator_runs SET snapshot = jsonb_set(snapshot, '{models,0,name}', '"tampered"') WHERE id = ?`, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := release.Publish(ctx, rel, target); err == nil {
		t.Error("the release was published again with different content")
	}
	if got := manifestDigest(t, target.Ref); got != tagged {
		t.Errorf("the tag moved to %s", got)
	}

	for what, stmt := range map[string]string{
		"republished": `UPDATE releases SET artifact_ref = 'elsewhere', published_at = now() WHERE id = ?`,
		"unpublished": `UPDATE releases SET artifact_ref = '', published_at = NULL WHERE id = ?`,
		"deleted":     `DELETE FROM releases WHERE id = ?`,
	} {
		if err := db.Exec(stmt, rel.ID).Error; err == nil {
			t.Errorf("the database accepted a published release %s", what)
		}
	}
}

// A release older than a published one is refused, so the tags never move back.
func TestPublishRefusesOlderRelease(t *testing.T) {
	s, db, older := generated(t, "release_older")
	if err := db.Exec(`UPDATE generator_runs SET snapshot = jsonb_set(snapshot, '{generated_at}', '"2026-01-01T00:00:00Z"') WHERE id = ?`, older.ID).Error; err != nil {
		t.Fatal(err)
	}
	newer, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	oldRel, newRel := create(t, s, older), create(t, s, newer)
	if oldRel.Version >= newRel.Version {
		t.Fatalf("release versions %s and %s are not in generation order", oldRel.Version, newRel.Version)
	}
	signer, _ := keySigner(t)
	host := registryHost(t)
	target := release.Target{Ref: host + "/neocloud/catalog:v4", OverlayRef: host + "/neocloud/overlay:v4", Signer: signer}
	if err := release.Publish(ctx, newRel, target); err != nil {
		t.Fatal(err)
	}
	tagged, overlay := manifestDigest(t, target.Ref), manifestDigest(t, target.OverlayRef)

	if err := release.Publish(ctx, oldRel, target); err == nil {
		t.Error("a release older than the published one was published")
	}
	if got, ov := manifestDigest(t, target.Ref), manifestDigest(t, target.OverlayRef); got != tagged || ov != overlay {
		t.Errorf("the tags moved back to %s and %s", got, ov)
	}
	if got, err := release.Get(ctx, s, oldRel.Version); err != nil || got.PublishedAt != nil {
		t.Errorf("the older release is recorded as published at %v (err %v)", got.PublishedAt, err)
	}
}

// A release published without its overlay publishes it later, once: again is a
// no-op, elsewhere is refused, the database refuses changes, and an older
// release's late overlay is refused.
func TestPublishOverlayLater(t *testing.T) {
	s, db, older := generated(t, "release_overlay_later")
	if err := db.Exec(`UPDATE generator_runs SET snapshot = jsonb_set(snapshot, '{generated_at}', '"2026-01-01T00:00:00Z"') WHERE id = ?`, older.ID).Error; err != nil {
		t.Fatal(err)
	}
	replaceOverlay(t, s, catalog.Overlay{Models: map[string]catalog.Extensions{older.Snapshot.Models[0].Slug: {"example.com/tier": json.RawMessage(`{"tier":"gold"}`)}}})
	newer, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	oldRel, newRel := create(t, s, older), create(t, s, newer)
	signer, _ := keySigner(t)
	host := registryHost(t)
	bare := release.Target{Ref: host + "/neocloud/catalog:v4", Signer: signer}
	full := bare
	full.OverlayRef = host + "/neocloud/overlay:v4"
	for _, rel := range []release.Release{oldRel, newRel} {
		if err := release.Publish(ctx, rel, bare); err != nil {
			t.Fatal(err)
		}
	}

	if err := release.Publish(ctx, oldRel, full); err == nil {
		t.Error("the overlay of a release older than the published one was published")
	}
	if err := release.Publish(ctx, newRel, full); err != nil {
		t.Fatal(err)
	}
	got, err := release.Get(ctx, s, newRel.Version)
	if err != nil {
		t.Fatal(err)
	}
	tagged := manifestDigest(t, full.OverlayRef)
	if got.OverlayRef != host+"/neocloud/overlay@"+tagged.String() {
		t.Fatalf("overlay recorded as %q, want the manifest the overlay tag names, %s", got.OverlayRef, tagged)
	}
	subject, err := name.NewDigest(got.OverlayRef)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(bundles(t, subject)); n != 1 {
		t.Fatalf("the late overlay carries %d signatures, want 1", n)
	}
	if err := release.Publish(ctx, newRel, full); err != nil || len(bundles(t, subject)) != 1 {
		t.Errorf("publishing the overlay again: %v, or it was signed again", err)
	}
	elsewhere := full
	elsewhere.OverlayRef = host + "/elsewhere/overlay:v4"
	if err := release.Publish(ctx, newRel, elsewhere); err == nil {
		t.Error("a published overlay was published again elsewhere")
	}
	if err := db.Exec(`UPDATE releases SET overlay_ref = 'elsewhere' WHERE id = ?`, newRel.ID).Error; err == nil {
		t.Error("the database accepted a changed overlay_ref")
	}
	if again, err := release.Get(ctx, s, newRel.Version); err != nil || again.OverlayRef != got.OverlayRef {
		t.Errorf("overlay recorded as %q after the refusals (err %v)", again.OverlayRef, err)
	}
}

// Keyless runs cosign as the release workflow does; a cosign failure carries its output.
func TestKeylessRunsCosign(t *testing.T) {
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" END >> \"$COSIGN_ARGV\"\n[ -z \"$COSIGN_FAIL\" ] || { echo \"$COSIGN_FAIL\" >&2; exit 1; }\n"
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("COSIGN_ARGV", argv)
	t.Setenv("COSIGN_FAIL", "")

	repo, err := name.NewRepository("registry.example/neocloud/catalog")
	if err != nil {
		t.Fatal(err)
	}
	digest := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("ab", 32)}
	var k release.Keyless
	if err := k.SignManifest(ctx, repo, v1.Descriptor{Digest: digest, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := k.SignBlob(ctx, "dist/SHA256SUMS", "dist/SHA256SUMS.sigstore.json"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"sign", "--yes", "registry.example/neocloud/catalog@" + digest.String(), "END",
		"sign-blob", "--yes", "--bundle", "dist/SHA256SUMS.sigstore.json", "dist/SHA256SUMS", "END", "",
	}, "\n")
	if string(got) != want {
		t.Errorf("cosign ran with\n%s\nwant\n%s", got, want)
	}

	t.Setenv("COSIGN_FAIL", "no OIDC token")
	if err := k.SignBlob(ctx, "dist/SHA256SUMS", "dist/SHA256SUMS.sigstore.json"); err == nil || !strings.Contains(err.Error(), "no OIDC token") {
		t.Errorf("a failing cosign: err = %v, want its output", err)
	}
	if err := k.SignManifest(ctx, repo, v1.Descriptor{Digest: digest, Size: 1}); err == nil || !strings.Contains(err.Error(), "no OIDC token") {
		t.Errorf("a failing cosign: err = %v, want its output", err)
	}
}

func replaceOverlay(t *testing.T, s *store.Store, ov catalog.Overlay) {
	t.Helper()
	run, err := s.StartImport(ctx, uuid.New(), "overlay-test", model.LayerOverlay, "overlay-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReplaceLayer(ctx, model.LayerOverlay, run.ID, store.Inputs{Extensions: ov}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishImport(ctx, run.ID, "", nil, nil); err != nil {
		t.Fatal(err)
	}
}

// A release pins the overlay as of its creation; later overlay changes do not affect it.
func TestReleasePinsOverlay(t *testing.T) {
	s, _, run := generated(t, "release_overlay")
	slug := run.Snapshot.Models[0].Slug
	payload := func(tier string) catalog.Overlay {
		return catalog.Overlay{Models: map[string]catalog.Extensions{slug: {"example.com/tier": json.RawMessage(`{"tier": "` + tier + `"}`)}}}
	}
	replaceOverlay(t, s, payload("gold"))
	rel := create(t, s, run)
	replaceOverlay(t, s, payload("silver"))

	got, err := release.Get(ctx, s, rel.Version)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := got.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Overlay.SnapshotDigest != rel.Digest || got.Overlay.SchemaVersion != catalog.SchemaVersion {
		t.Errorf("overlay pinned to %s at schema %s, want the release digest %s at %s",
			got.Overlay.SnapshotDigest, got.Overlay.SchemaVersion, rel.Digest, catalog.SchemaVersion)
	}
	merged, err := catalog.Merge(snap, got.Overlay)
	if err != nil {
		t.Fatal(err)
	}
	if p := string(merged.Models[0].Extensions["example.com/tier"]); !strings.Contains(p, "gold") {
		t.Errorf("merged payload = %s, want the overlay as of the release (gold)", p)
	}
	for _, m := range snap.Models {
		if len(m.Extensions) > 0 {
			t.Errorf("released snapshot model %s carries extensions", m.Slug)
		}
	}
}

// A published release and its SHA256SUMS verify as catalogsrc verifies a keyed
// catalog, with the signer's public key only.
func TestPublishedReleaseVerifiesWithCatalogsrcRules(t *testing.T) {
	s, _, run := generated(t, "release_verify")
	rel := create(t, s, run)
	signer, pub := keySigner(t)
	target := release.Target{Ref: registryHost(t) + "/neocloud/catalog:v4", Signer: signer}
	if err := release.Publish(ctx, rel, target); err != nil {
		t.Fatal(err)
	}

	version, raw, err := check(t, target.Ref, pub)
	if err != nil {
		t.Fatal(err)
	}
	var snap catalog.CatalogSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if version != rel.Version || snap.Digest() != rel.Digest {
		t.Errorf("verified version %s digest %s, want the release's %s %s", version, snap.Digest(), rel.Version, rel.Digest)
	}
	_, other := keySigner(t)
	if _, _, err := check(t, target.Ref, other); err == nil {
		t.Error("the release verified with another key")
	}

	dir := t.TempDir()
	if err := release.WriteExports(ctx, rel, dir, signer); err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := bundle.LoadJSONFromPath(filepath.Join(dir, "SHA256SUMS.sigstore.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyVerifier(t, pub).Verify(b, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(sums)), verify.WithKey())); err != nil {
		t.Errorf("SHA256SUMS bundle: %v", err)
	}
	if _, err := keyVerifier(t, other).Verify(b, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(sums)), verify.WithKey())); err == nil {
		t.Error("SHA256SUMS verified with another key")
	}
}

// Neither the published snapshot nor any export carries a price or cost, though
// the released run has cost estimates.
func TestReleaseExportHasNoPricesOrCosts(t *testing.T) {
	sqlDB, db := dbtest.Fresh(t, "release_no_prices")
	dbtest.Migrate(t, sqlDB)
	s := store.New(db)
	imp, err := bootstrap.New(filepath.Join(coreDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importers.Execute(ctx, s, imp, uuid.New()); err != nil {
		t.Fatal(err)
	}
	asOf := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	price := model.NodeTypePrice{Provenance: model.Provenance{Layer: model.LayerUpstream, Source: "azure-prices",
		SourceURL: "https://prices.azure.com/api/retail/prices", AsOf: asOf},
		NodeTypeID: "azure/Standard_ND96isr_H100_v5", Region: "eastus", Billing: model.BillingOnDemand, USDPerHour: 98.7654}
	if err := db.Create(&price).Error; err != nil {
		t.Fatal(err)
	}
	local := model.NodeTypePrice{NodeTypeID: "azure/Standard_ND96isr_MI300X_v5", Region: "eastus2", Billing: model.BillingSpot, USDPerHour: 12.3457}
	if err := s.ReplaceLocalPrices(ctx, []model.NodeTypePrice{local}); err != nil {
		t.Fatal(err)
	}
	run, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	var estimates []model.CostEstimate
	if err := db.Find(&estimates, "generator_run_id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	figures := map[string]bool{}
	for _, v := range []float64{price.USDPerHour, local.USDPerHour} {
		figures[regexp.QuoteMeta(strconv.FormatFloat(v, 'f', -1, 64))] = true
	}
	sources := map[string]bool{}
	for _, c := range estimates {
		sources[c.PriceSource] = true
		for _, v := range []float64{c.USDPerHour, c.USDPerMonth} {
			figures[regexp.QuoteMeta(strconv.FormatFloat(v, 'f', -1, 64))] = true
		}
	}
	if !sources["azure-prices"] || !sources[model.SourceCuration] {
		t.Fatalf("run estimated costs from %v, want the upstream and the local price", sources)
	}
	figure := regexp.MustCompile(strings.Join(slices.Sorted(maps.Keys(figures)), "|"))

	rel := create(t, s, run)
	signer, pub := keySigner(t)
	target := release.Target{Ref: registryHost(t) + "/neocloud/catalog:v4", Signer: signer}
	if err := release.Publish(ctx, rel, target); err != nil {
		t.Fatal(err)
	}
	_, raw, err := check(t, target.Ref, pub)
	if err != nil {
		t.Fatal(err)
	}
	internal := regexp.MustCompile(`(?i)price|cost|usd`)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, e := range v {
				if internal.MatchString(k) {
					t.Errorf("published snapshot has the key %s.%s", path, k)
				}
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	walk("$", doc)
	if f := figure.Find(raw); f != nil {
		t.Errorf("published snapshot carries the price or cost figure %s", f)
	}

	dir := t.TempDir()
	if err := release.WriteExports(ctx, rel, dir, signer); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), "SHA256SUMS") {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if internal.Match(body) || figure.Match(body) {
			t.Errorf("export %s carries a price or cost", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The bootstrap release matches the file-based pipeline: committed catalog.json
// and the adapters' goldens.
func TestBootstrapReleaseMatchesGoldens(t *testing.T) {
	s, _, run := generated(t, "release_goldens")
	rel := create(t, s, run)
	snap, err := rel.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(filepath.Join(coreDir, "snapshots/v4/catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c catalog.CatalogSnapshot
	if err := json.Unmarshal(committed, &c); err != nil {
		t.Fatal(err)
	}
	snap.Version, snap.GeneratedAt = c.Version, c.GeneratedAt
	if b, err := snap.Encode(); err != nil || !bytes.Equal(b, committed) {
		t.Errorf("release snapshot at the committed version does not encode as snapshots/v4/catalog.json (err %v)", err)
	}

	signer, _ := keySigner(t)
	dir := t.TempDir()
	if err := release.WriteExports(ctx, rel, dir, signer); err != nil {
		t.Fatal(err)
	}
	goldens := map[string]string{
		"gpustack":             "adapters/gpustack/testdata/gpustack",
		"airunway-recipes":     "adapters/airunway/testdata/airunway-recipes",
		"airunway-deployments": "adapters/airunway/testdata/airunway-deployments",
	}
	var golden []string
	for format, gdir := range goldens {
		gdir = filepath.Join(coreDir, gdir)
		err := filepath.WalkDir(gdir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(gdir, p)
			want, _ := os.ReadFile(p)
			got, err := os.ReadFile(filepath.Join(dir, format, rel))
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s/%s differs from the golden (err %v)", format, rel, err)
			}
			golden = append(golden, filepath.ToSlash(filepath.Join(format, rel)))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for line := range strings.Lines(string(sums)) {
		_, path, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "  ")
		listed = append(listed, path)
	}
	if len(listed) != len(golden) {
		t.Errorf("SHA256SUMS lists %d files, the goldens hold %d", len(listed), len(golden))
	}
}

// PlainHTTP publishes over HTTP to a registry whose name would get HTTPS.
func TestPublishPlainHTTP(t *testing.T) {
	s, _, run := generated(t, "release_plain_http")
	rel := create(t, s, run)
	signer, pub := keySigner(t)
	host := registryHost(t)
	tr := remote.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, host)
	}
	orig := remote.DefaultTransport
	t.Cleanup(func() { remote.DefaultTransport = orig })
	remote.DefaultTransport = tr
	target := release.Target{Ref: "registry.example/neocloud/catalog:v4", Signer: signer}
	if err := release.Publish(ctx, rel, target); err == nil {
		t.Fatal("published over HTTPS to a plain-HTTP registry")
	}
	target.PlainHTTP = true
	if err := release.Publish(ctx, rel, target); err != nil {
		t.Fatal(err)
	}
	if _, _, err := check(t, host+"/neocloud/catalog:v4", pub); err != nil {
		t.Fatal(err)
	}
}
