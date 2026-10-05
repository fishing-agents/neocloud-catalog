package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	catalog "github.com/fishman/neocloud-catalog"
)

func TestRunKeepsVersionAndSignsIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "snapshots/v3"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"models.json", "families.json", "backends.json"} {
		b, err := os.ReadFile(filepath.Join("../../data", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seed := make([]byte, ed25519.SeedSize)
	_, _ = rand.Read(seed)
	keyFile := filepath.Join(dir, "sign.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if err := run(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(indexPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("index written without a key: %v", err)
	}
	first, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(keyFile); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("regenerating unchanged data rewrote the snapshot")
	}

	var snap catalog.CatalogSnapshot
	var ix catalog.Index
	if err := loadJSON(snapshotPath, &snap); err != nil {
		t.Fatal(err)
	}
	if err := loadJSON(indexPath, &ix); err != nil {
		t.Fatal(err)
	}
	if err := catalog.VerifyIndex(ix, ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	if ix.Digest != snap.Digest() || ix.Version != snap.Version {
		t.Fatalf("index pins %s %s, snapshot is %s %s", ix.Version, ix.Digest, snap.Version, snap.Digest())
	}
}
