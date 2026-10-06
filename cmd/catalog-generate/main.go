// Command catalog-generate is the outsourced generation pipeline: it reads the
// curated sources in data/, validates them, derives the fit matrix, and emits
// the versioned snapshot at snapshots/v3/catalog.json that the module embeds and
// neocloud ingests. Re-run it after any change to data/ to bump the snapshot;
// with --sign-key it also writes the signed snapshots/v3/index.json.
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	catalog "github.com/fishman/neocloud-catalog"
)

const (
	snapshotPath = "snapshots/v3/catalog.json"
	indexPath    = "snapshots/v3/index.json"
)

func main() {
	signKey := flag.String("sign-key", "", "file holding the hex Ed25519 seed (32 bytes) that signs snapshots/v3/index.json")
	flag.Parse()
	if err := run(*signKey); err != nil {
		fmt.Fprintln(os.Stderr, "catalog-generate:", err)
		os.Exit(1)
	}
}

func run(signKey string) error {
	var priv ed25519.PrivateKey
	if signKey != "" {
		var err error
		if priv, err = loadKey(signKey); err != nil {
			return err
		}
	}
	var templates []catalog.ModelTemplate
	if err := loadJSON("data/models.json", &templates); err != nil {
		return err
	}
	var families []catalog.FamilyCapability
	if err := loadJSON("data/families.json", &families); err != nil {
		return err
	}
	var backends []catalog.Backend
	if err := loadJSON("data/backends.json", &backends); err != nil {
		return err
	}
	snap, err := catalog.Generate(templates, families, backends)
	if err != nil {
		return err
	}

	// Unchanged content keeps the committed version, so signing a release
	// reproduces the committed snapshot byte for byte.
	var prev catalog.CatalogSnapshot
	if err := loadJSON(snapshotPath, &prev); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	snap.Version, snap.GeneratedAt = prev.Version, prev.GeneratedAt
	if prev.Version == "" || snap.Digest() != prev.Digest() {
		snap.GeneratedAt = time.Now().UTC()
		snap.Version = snap.GeneratedAt.Format(catalog.VersionLayout)
	}
	if err := writeJSON(snapshotPath, snap); err != nil {
		return err
	}
	fmt.Printf("wrote %s (version=%s models=%d fits=%d backends=%d digest=%s)\n",
		snapshotPath, snap.Version, len(snap.Models), len(snap.Fits), len(snap.Backends), snap.Digest())
	if priv == nil {
		return nil
	}
	ix := catalog.Index{SchemaVersion: snap.SchemaVersion, Version: snap.Version, Digest: snap.Digest(), SnapshotURL: "catalog.json"}
	catalog.SignIndex(&ix, priv)
	if err := writeJSON(indexPath, ix); err != nil {
		return err
	}
	fmt.Printf("wrote %s (public key %s)\n", indexPath, hex.EncodeToString(priv.Public().(ed25519.PublicKey)))
	return nil
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("sign key %s: want %d hex-encoded bytes", path, ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}
