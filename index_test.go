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
	ix := Index{SchemaVersion: SchemaVersion, Version: "2026-10-06T00:00:00Z", Digest: "aaaa", SnapshotURL: "catalog.json"}
	SignIndex(&ix, priv)
	if err := VerifyIndex(ix, pub); err != nil {
		t.Fatalf("signed index: %v", err)
	}
	tampered := ix
	tampered.Digest = "bbbb"
	garbled := ix
	garbled.Signature = "not base64"
	newerClaim := ix
	newerClaim.Version = "2026-10-07T00:00:00Z"
	otherSchema := ix
	otherSchema.SchemaVersion = "9.0.0"
	for name, c := range map[string]struct {
		ix  Index
		pub ed25519.PublicKey
	}{
		"tampered digest": {tampered, pub},
		"edited version":  {newerClaim, pub},
		"edited schema":   {otherSchema, pub},
		"other key":       {ix, otherPub},
		"garbled":         {garbled, pub},
		"unsigned":        {Index{Digest: ix.Digest}, pub},
	} {
		if err := VerifyIndex(c.ix, c.pub); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
	freeForm := Index{SchemaVersion: SchemaVersion, Version: "v10", Digest: "aaaa", SnapshotURL: "catalog.json"}
	SignIndex(&freeForm, priv)
	if err := VerifyIndex(freeForm, pub); err == nil {
		t.Error("signed index with a free-form version verified; string ordering would accept v10 < v9")
	}
}
