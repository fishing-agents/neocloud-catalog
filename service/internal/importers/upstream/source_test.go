// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package upstream_test

// neocloud-api's catalogsrc test cases, run against Source.

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"google.golang.org/protobuf/encoding/protojson"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/importers/upstream"
	"github.com/fishman/neocloud-catalog/service/internal/release"
)

const (
	issuer   = "https://token.actions.githubusercontent.com"
	workflow = "https://github.com/fishing-agents/neocloud-catalog/.github/workflows/release.yml@refs/tags/v1.2.0"
	identity = `^https://github\.com/fishing-agents/neocloud-catalog/\.github/workflows/release\.yml@refs/tags/v.*$`
)

var ctx = context.Background()

func registryHost(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

type rawManifest []byte

func (r rawManifest) RawManifest() ([]byte, error) { return r, nil }

func (r rawManifest) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }

// pushArtifact pushes a single-layer artifact; a non-nil subject makes it a
// referrer.
func pushArtifact(t *testing.T, ref, artifactType, layerType string, body []byte, annotations map[string]string, subject *v1.Descriptor) v1.Descriptor {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	m := v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, ArtifactType: artifactType,
		Annotations: annotations, Subject: subject}
	for i, l := range []v1.Layer{static.NewLayer([]byte("{}"), "application/vnd.oci.empty.v1+json"), static.NewLayer(body, types.MediaType(layerType))} {
		if err := remote.WriteLayer(r.Context(), l); err != nil {
			t.Fatal(err)
		}
		d, err := l.Digest()
		if err != nil {
			t.Fatal(err)
		}
		size, _ := l.Size()
		mt, _ := l.MediaType()
		desc := v1.Descriptor{MediaType: mt, Digest: d, Size: size}
		if i == 0 {
			m.Config = desc
		} else {
			m.Layers = []v1.Descriptor{desc}
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(r, rawManifest(raw)); err != nil {
		t.Fatal(err)
	}
	d, _, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: d, Size: int64(len(raw)), ArtifactType: artifactType}
}

func pushSnapshot(t *testing.T, ref string, snap catalog.CatalogSnapshot) v1.Descriptor {
	t.Helper()
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	return pushArtifact(t, ref, catalog.ArtifactType, catalog.ArtifactType, body, map[string]string{catalog.VersionAnnotation: snap.Version}, nil)
}

func pushOverlay(t *testing.T, ref string, o catalog.Overlay) v1.Descriptor {
	t.Helper()
	body, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return pushArtifact(t, ref, catalog.OverlayArtifactType, catalog.OverlayArtifactType, body, nil, nil)
}

// attach attaches a Sigstore bundle as cosign v3 does.
func attach(t *testing.T, repo string, subject v1.Descriptor, bundleJSON []byte) {
	t.Helper()
	pushArtifact(t, repo+":bundle-"+subject.Digest.Hex[:12], release.BundleType, release.BundleType, bundleJSON, nil, &subject)
}

func statement(t *testing.T, digest v1.Hash) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"subject":       []map[string]any{{"digest": map[string]string{digest.Algorithm: digest.Hex}}},
		"predicateType": "https://sigstore.dev/cosign/sign/v1",
		"predicate":     map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// keylessBundle signs digest and serializes it as cosign attaches it: a v0.3
// bundle.
func keylessBundle(t *testing.T, vs *ca.VirtualSigstore, san, iss string, digest v1.Hash) []byte {
	t.Helper()
	e, err := vs.AttestAtTime(san, iss, statement(t, digest), time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	vc, err := e.VerificationContent()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := e.SignatureContent()
	if err != nil {
		t.Fatal(err)
	}
	env := sc.(*sgbundle.Envelope).Envelope
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signatures[0].Sig)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := e.TlogEntries()
	if err != nil {
		t.Fatal(err)
	}
	// A virtual entry's proto form lacks the kind and SET; rebuild both as Rekor does.
	tles := make([]*protorekor.TransparencyLogEntry, len(entries))
	for i, en := range entries {
		tle := en.TransparencyLogEntry()
		var kind struct {
			Kind       string `json:"kind"`
			APIVersion string `json:"apiVersion"`
		}
		if err := json.Unmarshal(tle.CanonicalizedBody, &kind); err != nil {
			t.Fatal(err)
		}
		tle.KindVersion = &protorekor.KindVersion{Kind: kind.Kind, Version: kind.APIVersion}
		set, err := vs.RekorSignPayload(tlog.RekorPayload{Body: base64.StdEncoding.EncodeToString(tle.CanonicalizedBody),
			IntegratedTime: tle.IntegratedTime, LogIndex: tle.LogIndex, LogID: hex.EncodeToString(tle.LogId.KeyId)})
		if err != nil {
			t.Fatal(err)
		}
		tle.InclusionPromise = &protorekor.InclusionPromise{SignedEntryTimestamp: set}
		tles[i] = tle
	}
	raw, err := protojson.Marshal(&protobundle.Bundle{
		MediaType: release.BundleType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content:     &protobundle.VerificationMaterial_Certificate{Certificate: &protocommon.X509Certificate{RawBytes: vc.Certificate().Raw}},
			TlogEntries: tles,
		},
		Content: &protobundle.Bundle_DsseEnvelope{DsseEnvelope: &protodsse.Envelope{
			Payload: payload, PayloadType: env.PayloadType, Signatures: []*protodsse.Signature{{Sig: sig}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func keyPair(t *testing.T) (release.Signer, string) {
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
	path := filepath.Join(t.TempDir(), "cosign.pub")
	if err := os.WriteFile(path, pub, 0o600); err != nil {
		t.Fatal(err)
	}
	return signer, path
}

func sign(t *testing.T, signer release.Signer, repo string, subject v1.Descriptor) {
	t.Helper()
	r, err := name.NewRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.SignManifest(ctx, r, subject); err != nil {
		t.Fatal(err)
	}
}

func newSource(t *testing.T, cfg upstream.Config) *upstream.Source {
	t.Helper()
	s, err := upstream.NewSource(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func committedSnapshot(t *testing.T) catalog.CatalogSnapshot {
	t.Helper()
	snap, err := catalog.SnapshotV4()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func keyless(t *testing.T, ref string, vs *ca.VirtualSigstore) *upstream.Source {
	t.Helper()
	s := newSource(t, upstream.Config{Ref: ref, IdentityIssuer: issuer, Identity: identity})
	upstream.TrustForTest(s, vs, 0)
	return s
}

func TestKeylessCheckAndPull(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	repo := registryHost(t) + "/neocloud-catalog"
	snap := committedSnapshot(t)
	desc := pushSnapshot(t, repo+":v4", snap)
	attach(t, repo, desc, keylessBundle(t, vs, workflow, issuer, desc.Digest))

	s := keyless(t, repo+":v4", vs)
	rel, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rel.Digest != desc.Digest || rel.Version != snap.Version {
		t.Fatalf("release = %s %s, want %s %s", rel.Digest, rel.Version, desc.Digest, snap.Version)
	}
	got, err := s.Pull(ctx, rel)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got.Digest() != snap.Digest() {
		t.Fatalf("pulled digest %s, want %s", got.Digest(), snap.Digest())
	}
}

func TestCheckRefuses(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	host := registryHost(t)
	snap := committedSnapshot(t)
	other := snap
	other.Version = "2026-01-01T00:00:00Z"
	for _, c := range []struct {
		name, wantErr string
		publish       func(repo string)
	}{
		{"unsigned", "not signed", func(repo string) { pushSnapshot(t, repo+":v4", snap) }},
		{"other workflow in the org", "SAN", func(repo string) {
			d := pushSnapshot(t, repo+":v4", snap)
			attach(t, repo, d, keylessBundle(t, vs, "https://github.com/fishing-agents/other/.github/workflows/release.yml@refs/tags/v1", issuer, d.Digest))
		}},
		{"identity as a suffix", "SAN", func(repo string) {
			d := pushSnapshot(t, repo+":v4", snap)
			attach(t, repo, d, keylessBundle(t, vs, "https://evil.example/"+workflow, issuer, d.Digest))
		}},
		{"other issuer", "issuer", func(repo string) {
			d := pushSnapshot(t, repo+":v4", snap)
			attach(t, repo, d, keylessBundle(t, vs, workflow, "https://accounts.google.com", d.Digest))
		}},
		{"signature of another manifest", "digest", func(repo string) {
			signed := pushSnapshot(t, repo+":old", other)
			d := pushSnapshot(t, repo+":v4", snap)
			attach(t, repo, d, keylessBundle(t, vs, workflow, issuer, signed.Digest))
		}},
		{"signed manifest without a valid version", "layout", func(repo string) {
			body, _ := json.Marshal(snap)
			d := pushArtifact(t, repo+":v4", catalog.ArtifactType, catalog.ArtifactType, body,
				map[string]string{catalog.VersionAnnotation: "v10"}, nil)
			attach(t, repo, d, keylessBundle(t, vs, workflow, issuer, d.Digest))
		}},
		{"signed manifest of another artifact type", "want one", func(repo string) {
			d := pushArtifact(t, repo+":v4", "application/vnd.example+json", catalog.ArtifactType, []byte("{}"),
				map[string]string{catalog.VersionAnnotation: snap.Version}, nil)
			attach(t, repo, d, keylessBundle(t, vs, workflow, issuer, d.Digest))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo := host + "/" + strings.ReplaceAll(c.name, " ", "-")
			c.publish(repo)
			_, err := keyless(t, repo+":v4", vs).Check(ctx)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("check err = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

func TestPullRefusesVersionMismatch(t *testing.T) {
	signer, pub := keyPair(t)
	repo := registryHost(t) + "/neocloud-catalog"
	body, _ := json.Marshal(committedSnapshot(t))
	d := pushArtifact(t, repo+":v4", catalog.ArtifactType, catalog.ArtifactType, body,
		map[string]string{catalog.VersionAnnotation: "2099-01-01T00:00:00Z"}, nil)
	sign(t, signer, repo, d)
	s := newSource(t, upstream.Config{Ref: repo + ":v4", PublicKey: pub})
	rel, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if _, err := s.Pull(ctx, rel); err == nil || !strings.Contains(err.Error(), "does not match the signed version") {
		t.Fatalf("pull err = %v, want a version mismatch", err)
	}
}

func TestKeyed(t *testing.T) {
	signer, pub := keyPair(t)
	_, stranger := keyPair(t)
	repo := registryHost(t) + "/neocloud-catalog"
	sign(t, signer, repo, pushSnapshot(t, repo+":v4", committedSnapshot(t)))
	for name, c := range map[string]struct {
		key string
		ok  bool
	}{"publisher key": {pub, true}, "other key": {stranger, false}} {
		if _, err := newSource(t, upstream.Config{Ref: repo + ":v4", PublicKey: c.key}).Check(ctx); (err == nil) != c.ok {
			t.Errorf("%s: check err = %v, want ok %v", name, err, c.ok)
		}
	}
}

// An overlay source checks and pulls only the overlay; a snapshot source
// refuses it.
func TestOverlayCheckAndPull(t *testing.T) {
	publisher, publisherPub := keyPair(t)
	enterprise, enterprisePub := keyPair(t)
	host := registryHost(t)
	snap := committedSnapshot(t)
	o := catalog.Overlay{SchemaVersion: catalog.SchemaVersion, SnapshotDigest: snap.Digest(),
		Models: map[string]catalog.Extensions{snap.Models[0].Slug: {"example.com/test": json.RawMessage(`{"tier":"gold"}`)}}}
	d := pushOverlay(t, host+"/overlay:v4", o)
	sign(t, enterprise, host+"/overlay", d)
	sign(t, publisher, host+"/neocloud-catalog", pushSnapshot(t, host+"/neocloud-catalog:v4", snap))

	overlay := func(ref, key string) *upstream.Source {
		return newSource(t, upstream.Config{Ref: ref, PublicKey: key, Overlay: true})
	}
	s := overlay(host+"/overlay:v4", enterprisePub)
	rel, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rel.Digest != d.Digest {
		t.Fatalf("overlay release %s, want %s", rel.Digest, d.Digest)
	}
	got, err := s.PullOverlay(ctx, rel)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !reflect.DeepEqual(got, o) {
		t.Fatalf("pulled overlay %+v, want %+v", got, o)
	}

	if _, err := overlay(host+"/overlay:v4", publisherPub).Check(ctx); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("overlay signed by another key: err = %v, want a signature error", err)
	}
	if _, err := overlay(host+"/neocloud-catalog:v4", publisherPub).Check(ctx); err == nil || !strings.Contains(err.Error(), catalog.OverlayArtifactType) {
		t.Errorf("overlay source on the snapshot: err = %v, want one naming %s", err, catalog.OverlayArtifactType)
	}
	sign(t, publisher, host+"/overlay", d)
	pub := newSource(t, upstream.Config{Ref: host + "/overlay:v4", PublicKey: publisherPub})
	if _, err := pub.Check(ctx); err == nil || !strings.Contains(err.Error(), catalog.ArtifactType) {
		t.Errorf("snapshot source on the overlay: err = %v, want one naming %s", err, catalog.ArtifactType)
	}
}

func TestNewRefusesUnsafeTrust(t *testing.T) {
	key := filepath.Join(t.TempDir(), "k.pub")
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]upstream.Config{
		"no trust":         {Ref: "ghcr.io/x/c:v4"},
		"issuer only":      {Ref: "ghcr.io/x/c:v4", IdentityIssuer: issuer},
		"unanchored":       {Ref: "ghcr.io/x/c:v4", IdentityIssuer: issuer, Identity: `https://github\.com/fishing-agents/.*`},
		"key and identity": {Ref: "ghcr.io/x/c:v4", IdentityIssuer: issuer, Identity: identity, PublicKey: key},
		"unparsable key":   {Ref: "ghcr.io/x/c:v4", PublicKey: key},
	} {
		if _, err := upstream.NewSource(cfg); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	if s, err := upstream.NewSource(upstream.Config{}); s != nil || err != nil {
		t.Errorf("empty ref = %v, %v; want nil, nil (no upstream)", s, err)
	}
	if _, err := upstream.NewSource(upstream.Config{Ref: "ghcr.io/x/o:v4", Overlay: true}); err == nil ||
		!strings.Contains(err.Error(), "upstream.overlay_ref needs upstream.overlay_public_key") {
		t.Errorf("overlay without trust: err = %v, want one naming the upstream.overlay_* keys", err)
	}
	if _, err := upstream.NewSource(upstream.Config{Ref: "ghcr.io/x/c:v4", IdentityIssuer: issuer, Identity: identity, TrustedRoot: key}); err == nil ||
		!strings.Contains(err.Error(), "upstream.trusted_root") {
		t.Errorf("unreadable trusted root: err = %v, want one naming upstream.trusted_root", err)
	}
}
