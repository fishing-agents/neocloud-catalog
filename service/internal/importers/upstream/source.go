package upstream

import (
	"bytes"
	"context"
	"crypto"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/release"
)

// maxBlobBytes bounds a snapshot or bundle download.
const maxBlobBytes = 64 << 20

// Config selects an upstream artifact and whom to trust, as neocloud-api's
// catalog.* keys do. Keyless (IdentityIssuer and Identity) trusts the
// upstream's release workflow; PublicKey trusts a cosign key pair instead.
type Config struct {
	// Ref is the artifact reference, for example ghcr.io/fishing-agents/neocloud-catalog:v4.
	Ref string
	// IdentityIssuer is the exact OIDC issuer of the signing certificate.
	IdentityIssuer string
	// Identity is an anchored regexp (^...$) over the certificate's subject
	// alternative name: the release workflow, never an org-wide pattern.
	Identity string
	// PublicKey is a PEM file holding a cosign public key.
	PublicKey string
	// TrustedRoot is a Sigstore trusted_root.json file for offline (air-gapped)
	// keyless verification; empty fetches the public-good root through TUF.
	TrustedRoot string
	// PlainHTTP talks HTTP to the registry, for a mirror without TLS.
	PlainHTTP bool
	// Overlay reads the overlay artifact (catalog.OverlayArtifactType),
	// configured under upstream.overlay_*, instead of the snapshot.
	Overlay bool
}

// key names cfg's configuration key in errors.
func (c Config) key(name string) string {
	if c.Overlay {
		return "upstream.overlay_" + name
	}
	return "upstream." + name
}

// Release is an upstream manifest whose signature verified. An overlay release
// has no version: its manifest digest identifies it.
type Release struct {
	Digest  v1.Hash
	Version string
	Time    time.Time
	layer   v1.Descriptor
}

// Source is a configured upstream artifact. It verifies what neocloud-api's
// catalogsrc verifies, the same way: Check verifies a signature on the
// manifest digest before trusting anything in the manifest; Pull and
// PullOverlay download what the verified manifest pins.
type Source struct {
	ref      name.Reference
	opts     []remote.Option
	material func() (root.TrustedMaterial, error)
	identity verify.PolicyOption
	keyless  bool
	// scts is how many certificate-transparency timestamps a keyless signing
	// certificate must embed; Fulcio's always carry one.
	scts    int
	overlay bool
}

// NewSource validates cfg and returns its Source, or nil when cfg.Ref is empty.
// Registry auth comes from the default keychain.
func NewSource(cfg Config) (*Source, error) {
	if cfg.Ref == "" {
		return nil, nil
	}
	var nameOpts []name.Option
	if cfg.PlainHTTP {
		nameOpts = append(nameOpts, name.Insecure)
	}
	ref, err := name.ParseReference(cfg.Ref, nameOpts...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.key("ref"), err)
	}
	s := &Source{ref: ref, opts: []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)}, overlay: cfg.Overlay}
	switch {
	case cfg.PublicKey != "" && (cfg.Identity != "" || cfg.IdentityIssuer != ""):
		return nil, fmt.Errorf("%s: set either %s or %s and %s, not both", cfg.key("ref"), cfg.key("public_key"), cfg.key("identity_issuer"), cfg.key("identity"))
	case cfg.PublicKey != "":
		pem, err := os.ReadFile(cfg.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cfg.key("public_key"), err)
		}
		pub, err := cryptoutils.UnmarshalPEMToPublicKey(pem)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cfg.key("public_key"), err)
		}
		v, err := signature.LoadVerifier(pub, crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cfg.key("public_key"), err)
		}
		tm := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) {
			return root.NewExpiringKey(v, time.Time{}, time.Time{}), nil
		})
		s.material = func() (root.TrustedMaterial, error) { return tm, nil }
		s.identity = verify.WithKey()
	case cfg.IdentityIssuer != "" && cfg.Identity != "":
		if !strings.HasPrefix(cfg.Identity, "^") || !strings.HasSuffix(cfg.Identity, "$") {
			return nil, fmt.Errorf("%s: anchor the regexp with ^ and $; an unanchored pattern matches foreign identities", cfg.key("identity"))
		}
		id, err := verify.NewShortCertificateIdentity(cfg.IdentityIssuer, "", "", cfg.Identity)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cfg.key("identity"), err)
		}
		s.identity = verify.WithCertificateIdentity(id)
		s.keyless, s.scts = true, 1
		if cfg.TrustedRoot != "" {
			tr, err := root.NewTrustedRootFromPath(cfg.TrustedRoot)
			if err != nil {
				return nil, fmt.Errorf("upstream.trusted_root: %w", err)
			}
			s.material = func() (root.TrustedMaterial, error) { return tr, nil }
		} else {
			// Fetched per check, so a Sigstore key rotation reaches running
			// services; in memory, as the pod's filesystem may be read-only.
			s.material = func() (root.TrustedMaterial, error) {
				return root.FetchTrustedRootWithOptions(tuf.DefaultOptions().WithDisableLocalCache())
			}
		}
	default:
		return nil, fmt.Errorf("%s needs %s, or %s and %s", cfg.key("ref"), cfg.key("public_key"), cfg.key("identity_issuer"), cfg.key("identity"))
	}
	return s, nil
}

// Ref is the configured reference.
func (s *Source) Ref() string { return s.ref.String() }

// Check resolves the reference, verifies a signature on its manifest digest,
// and returns the release the signed manifest describes.
func (s *Source) Check(ctx context.Context) (Release, error) {
	opts := append([]remote.Option{remote.WithContext(ctx)}, s.opts...)
	desc, err := remote.Get(s.ref, opts...)
	if err != nil {
		return Release{}, fmt.Errorf("manifest: %w", err)
	}
	if err := s.verify(desc.Digest, opts); err != nil {
		return Release{}, err
	}
	var m v1.Manifest
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return Release{}, fmt.Errorf("manifest: %w", err)
	}
	want := catalog.ArtifactType
	if s.overlay {
		want = catalog.OverlayArtifactType
	}
	if m.ArtifactType != want || len(m.Layers) != 1 || string(m.Layers[0].MediaType) != want {
		return Release{}, fmt.Errorf("manifest: want one %s layer in an artifact of that type", want)
	}
	if s.overlay {
		return Release{Digest: desc.Digest, layer: m.Layers[0]}, nil
	}
	version := m.Annotations[catalog.VersionAnnotation]
	t, err := catalog.ParseVersion(version)
	if err != nil {
		return Release{}, fmt.Errorf("manifest: %w", err)
	}
	return Release{Digest: desc.Digest, Version: version, Time: t, layer: m.Layers[0]}, nil
}

// verify accepts the manifest digest when any attached bundle verifies.
func (s *Source) verify(digest v1.Hash, ropts []remote.Option) error {
	tm, err := s.material()
	if err != nil {
		return fmt.Errorf("signature: trusted root: %w", err)
	}
	// A key is trusted by itself; a keyless certificate is short-lived, so its
	// signature must be logged in Rekor while it was valid.
	opts := []verify.VerifierOption{verify.WithNoObserverTimestamps()}
	if s.keyless {
		opts = []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
		if s.scts > 0 {
			opts = append(opts, verify.WithSignedCertificateTimestamps(s.scts))
		}
	}
	v, err := verify.NewVerifier(tm, opts...)
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	want, err := hex.DecodeString(digest.Hex)
	if err != nil {
		return err
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest(digest.Algorithm, want), s.identity)

	repo := s.ref.Context()
	idx, err := remote.Referrers(repo.Digest(digest.String()), append(ropts, remote.WithFilter("artifactType", release.BundleType))...)
	if err != nil {
		return fmt.Errorf("signature: referrers: %w", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return fmt.Errorf("signature: referrers: %w", err)
	}
	var errs []error
	for _, d := range im.Manifests {
		if d.ArtifactType != release.BundleType {
			continue
		}
		b, err := s.bundle(repo.Digest(d.Digest.String()), ropts)
		if err == nil {
			_, err = v.Verify(b, policy)
		}
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return errors.New("signature: the manifest is not signed")
	}
	return fmt.Errorf("signature: %w", errors.Join(errs...))
}

func (s *Source) bundle(ref name.Digest, opts []remote.Option) (*bundle.Bundle, error) {
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return nil, err
	}
	var m v1.Manifest
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return nil, err
	}
	if len(m.Layers) != 1 || m.Layers[0].MediaType != release.BundleType {
		return nil, fmt.Errorf("bundle %s: want one %s layer", ref.DigestStr(), release.BundleType)
	}
	raw, err := s.blob(ref.Context(), m.Layers[0], opts)
	if err != nil {
		return nil, err
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("bundle %s: %w", ref.DigestStr(), err)
	}
	return &b, nil
}

// Pull downloads the snapshot of a checked release, checked against the layer
// digest in the signed manifest.
func (s *Source) Pull(ctx context.Context, r Release) (catalog.CatalogSnapshot, error) {
	var snap catalog.CatalogSnapshot
	raw, err := s.blob(s.ref.Context(), r.layer, append([]remote.Option{remote.WithContext(ctx)}, s.opts...))
	if err != nil {
		return snap, fmt.Errorf("snapshot: %w", err)
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return snap, fmt.Errorf("snapshot: %w", err)
	}
	if snap.Version != r.Version {
		return snap, fmt.Errorf("snapshot version %s does not match the signed version %s", snap.Version, r.Version)
	}
	return snap, nil
}

// PullOverlay downloads the overlay of a checked overlay release, checked
// against the layer digest in the signed manifest.
func (s *Source) PullOverlay(ctx context.Context, r Release) (catalog.Overlay, error) {
	var o catalog.Overlay
	raw, err := s.blob(s.ref.Context(), r.layer, append([]remote.Option{remote.WithContext(ctx)}, s.opts...))
	if err != nil {
		return o, fmt.Errorf("overlay: %w", err)
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, fmt.Errorf("overlay: %w", err)
	}
	return o, nil
}

func (s *Source) blob(repo name.Repository, d v1.Descriptor, opts []remote.Option) ([]byte, error) {
	if d.Size > maxBlobBytes {
		return nil, fmt.Errorf("blob %s: %d bytes exceeds %d", d.Digest, d.Size, maxBlobBytes)
	}
	l, err := remote.Layer(repo.Digest(d.Digest.String()), opts...)
	if err != nil {
		return nil, err
	}
	rc, err := l.Compressed()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, d.Size+1))
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", d.Digest, err)
	}
	if int64(len(raw)) != d.Size {
		return nil, fmt.Errorf("blob %s: got %d bytes, manifest says %d", d.Digest, len(raw), d.Size)
	}
	if h, _, _ := v1.SHA256(bytes.NewReader(raw)); h != d.Digest {
		return nil, fmt.Errorf("blob %s: content digest %s", d.Digest, h)
	}
	return raw, nil
}
