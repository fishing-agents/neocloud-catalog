package catalog

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
)

// Index is the signed pointer an upstream publishes next to a snapshot. Clients
// fetch the index, verify its signature with the publisher's public key, fetch
// SnapshotURL (absolute, or relative to the index), and accept the snapshot only
// if its Digest matches. Anything else keeps the last good snapshot.
type Index struct {
	SchemaVersion string `json:"schema_version"`
	Version       string `json:"version"`
	Digest        string `json:"digest"`
	SnapshotURL   string `json:"snapshot_url"`
	Signature     string `json:"signature"`
}

// ErrBadSignature means the index signature does not verify with the key.
var ErrBadSignature = errors.New("catalog: index signature does not verify")

// SignIndex sets the index signature: Ed25519 over the digest, base64 (std).
func SignIndex(ix *Index, priv ed25519.PrivateKey) {
	ix.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(ix.Digest)))
}

// VerifyIndex checks the index signature against the publisher's public key.
func VerifyIndex(ix Index, pub ed25519.PublicKey) error {
	sig, err := base64.StdEncoding.DecodeString(ix.Signature)
	if err != nil || !ed25519.Verify(pub, []byte(ix.Digest), sig) {
		return ErrBadSignature
	}
	return nil
}
