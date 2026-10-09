// Command catalog-generate is the outsourced generation pipeline: it reads the
// curated sources in data/, validates them, derives the fit matrix, and emits
// the versioned snapshot at snapshots/v4/catalog.json that the module embeds and
// neocloud ingests. Re-run it after any change to data/ to bump the snapshot.
// Publishing and signing happen in the release workflow, not here.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	catalog "github.com/fishman/neocloud-catalog"
)

const snapshotPath = "snapshots/v4/catalog.json"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "catalog-generate:", err)
		os.Exit(1)
	}
}

func run() error {
	var templates []catalog.ModelTemplate
	if err := loadJSON("data/models.json", &templates); err != nil {
		return err
	}
	var drafts []catalog.DraftModel
	if err := loadJSON("data/draft_models.json", &drafts); err != nil {
		return err
	}
	var families []catalog.Family
	if err := loadJSON("data/families.json", &families); err != nil {
		return err
	}
	var backends []catalog.Backend
	if err := loadJSON("data/backends.json", &backends); err != nil {
		return err
	}
	var cloud []catalog.NodeType
	if err := loadJSON("data/node_types.json", &cloud); err != nil {
		return err
	}
	snap, report, err := catalog.Generate(families, cloud, templates, drafts, backends)
	if err != nil {
		return err
	}
	for _, line := range report {
		fmt.Println(line)
	}

	// Unchanged content keeps the committed version, so the release workflow
	// reproduces the committed snapshot byte for byte.
	prev, changed, err := previous(snapshotPath)
	if err != nil {
		return err
	}
	if changed != "" {
		fmt.Println(changed)
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
	return nil
}

// previous reads the committed snapshot. One that does not decode or has
// another schema major.minor is changed content: it returns an empty snapshot
// and the reason, so a shape change gets a new version instead of failing.
func previous(path string) (catalog.CatalogSnapshot, string, error) {
	var prev catalog.CatalogSnapshot
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return prev, "", nil
	}
	if err != nil {
		return prev, "", err
	}
	if err := json.Unmarshal(b, &prev); err != nil {
		return catalog.CatalogSnapshot{}, fmt.Sprintf("previous %s does not decode (%v): new version", path, err), nil
	}
	if majorMinor(prev.SchemaVersion) != majorMinor(catalog.SchemaVersion) {
		return catalog.CatalogSnapshot{}, fmt.Sprintf("previous %s has schema %q, not %s: new version", path, prev.SchemaVersion, catalog.SchemaVersion), nil
	}
	return prev, "", nil
}

func majorMinor(v string) string {
	major, rest, _ := strings.Cut(v, ".")
	minor, _, _ := strings.Cut(rest, ".")
	return major + "." + minor
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
