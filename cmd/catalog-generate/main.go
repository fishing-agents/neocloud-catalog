// Command catalog-generate is the outsourced generation pipeline: it reads the
// curated sources in data/, derives the fit matrix, and emits the versioned
// snapshot at snapshots/v2/catalog.json that the module embeds and neocloud
// ingests. Re-run it after any change to data/ to bump the snapshot.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	catalog "github.com/fishman/neocloud-catalog"
)

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
	var families []catalog.FamilyCapability
	if err := loadJSON("data/families.json", &families); err != nil {
		return err
	}

	snap := catalog.Generate(templates, families)
	snap.Version = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	snap.GeneratedAt = time.Now().UTC()

	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll("snapshots/v2", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile("snapshots/v2/catalog.json", append(out, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote snapshots/v2/catalog.json (version=%s models=%d fits=%d digest=%s)\n",
		snap.Version, len(snap.Models), len(snap.Fits), snap.Digest())
	return nil
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
