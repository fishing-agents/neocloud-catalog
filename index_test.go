package catalog

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

func TestSignIndexVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ix := Index{SchemaVersion: SchemaVersion, Version: "v1", Digest: "aaaa", SnapshotURL: "catalog.json"}
	SignIndex(&ix, priv)
	if err := VerifyIndex(ix, pub); err != nil {
		t.Fatalf("signed index: %v", err)
	}
	tampered := ix
	tampered.Digest = "bbbb"
	garbled := ix
	garbled.Signature = "not base64"
	for name, c := range map[string]struct {
		ix  Index
		pub ed25519.PublicKey
	}{
		"tampered digest": {tampered, pub},
		"other key":       {ix, otherPub},
		"garbled":         {garbled, pub},
		"unsigned":        {Index{Digest: ix.Digest}, pub},
	} {
		if err := VerifyIndex(c.ix, c.pub); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}
