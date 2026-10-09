package release

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	"google.golang.org/protobuf/encoding/protojson"
)

// Signer signs in the formats cosign v3 writes and catalogsrc verifies.
type Signer interface {
	// SignManifest attaches a bundle as an OCI referrer of type BundleType, as cosign sign does.
	SignManifest(ctx context.Context, repo name.Repository, manifest v1.Descriptor) error
	// SignBlob writes a bundle over path to bundle, as cosign sign-blob --bundle does.
	SignBlob(ctx context.Context, path, bundle string) error
}

// KeySigner signs offline with a cosign key pair: bundles carry the key hint
// and no transparency-log entry, which catalogsrc accepts for a public key.
type KeySigner struct{ kp keypair }

// NewKeySigner loads a cosign.key encrypted with password, or an unencrypted PKCS#8 key.
func NewKeySigner(keyPEM, password []byte) (*KeySigner, error) {
	priv, err := cryptoutils.UnmarshalPEMToPrivateKey(keyPEM, cryptoutils.StaticPasswordFunc(password))
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	signer, ok := priv.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("signing key: a %T cannot sign", priv)
	}
	alg, err := signature.GetDefaultAlgorithmDetails(signer.Public())
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	h := sha256.Sum256(der)
	return &KeySigner{keypair{signer, alg, []byte(base64.StdEncoding.EncodeToString(h[:]))}}, nil
}

// SignManifest signs an in-toto statement on the manifest digest, as cosign does for images.
func (k *KeySigner) SignManifest(ctx context.Context, repo name.Repository, manifest v1.Descriptor) error {
	stmt, err := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"subject":       []map[string]any{{"digest": map[string]string{manifest.Digest.Algorithm: manifest.Digest.Hex}}},
		"predicateType": "https://sigstore.dev/cosign/sign/v1",
		"predicate":     map[string]any{},
	})
	if err != nil {
		return err
	}
	b, err := k.bundle(ctx, &sign.DSSEData{Data: stmt, PayloadType: "application/vnd.in-toto+json"})
	if err != nil {
		return err
	}
	subject := v1.Descriptor{MediaType: manifest.MediaType, Digest: manifest.Digest, Size: manifest.Size}
	a, err := newArtifact(BundleType, "", b, nil, &subject)
	if err != nil {
		return err
	}
	return a.push(repo, remoteOptions(ctx))
}

func (k *KeySigner) SignBlob(ctx context.Context, path, bundle string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	b, err := k.bundle(ctx, &sign.PlainData{Data: data})
	if err != nil {
		return err
	}
	return os.WriteFile(bundle, b, 0o644)
}

func (k *KeySigner) bundle(ctx context.Context, content sign.Content) ([]byte, error) {
	b, err := sign.Bundle(content, k.kp, sign.BundleOptions{Context: ctx})
	if err != nil {
		return nil, err
	}
	return protojson.Marshal(b)
}

// keypair is a sign.Keypair of a loaded key; sigstore-go only generates ephemeral ones.
type keypair struct {
	priv crypto.Signer
	alg  signature.AlgorithmDetails
	hint []byte
}

func (k keypair) GetHashAlgorithm() protocommon.HashAlgorithm { return k.alg.GetProtoHashType() }
func (k keypair) GetSigningAlgorithm() protocommon.PublicKeyDetails {
	return k.alg.GetSignatureAlgorithm()
}
func (k keypair) GetHint() []byte                { return k.hint }
func (k keypair) GetPublicKey() crypto.PublicKey { return k.priv.Public() }

func (k keypair) GetKeyAlgorithm() string {
	switch k.alg.GetKeyType() {
	case signature.ECDSA:
		return "ECDSA"
	case signature.RSA:
		return "RSA"
	case signature.ED25519:
		return "ED25519"
	}
	return ""
}

func (k keypair) GetPublicKeyPem() (string, error) {
	pem, err := cryptoutils.MarshalPublicKeyToPEM(k.priv.Public())
	return string(pem), err
}

// SignData signs the digest of data (data itself for pure Ed25519) and returns
// the signature and what it signed.
func (k keypair) SignData(_ context.Context, data []byte) ([]byte, []byte, error) {
	hf := k.alg.GetHashType()
	signed := data
	if hf != crypto.Hash(0) {
		h := hf.New()
		h.Write(data)
		signed = h.Sum(nil)
	}
	sig, err := k.priv.Sign(rand.Reader, signed, hf)
	return sig, signed, err
}

// Keyless runs cosign's keyless flow (CI only), binding content to the
// workflow's OIDC identity. Registry auth is cosign's own.
type Keyless struct{}

func (Keyless) SignManifest(ctx context.Context, repo name.Repository, manifest v1.Descriptor) error {
	return cosign(ctx, "sign", "--yes", repo.Digest(manifest.Digest.String()).String())
}

func (Keyless) SignBlob(ctx context.Context, path, bundle string) error {
	return cosign(ctx, "sign-blob", "--yes", "--bundle", bundle, path)
}

func cosign(ctx context.Context, args ...string) error {
	if out, err := exec.CommandContext(ctx, "cosign", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("cosign %s: %w: %s", args[0], err, out)
	}
	return nil
}
