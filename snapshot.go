package catalog

import (
	"embed"
	"encoding/json"
	"fmt"
)

//go:embed snapshots/v3/catalog.json
var snapshotFS embed.FS

// SnapshotV3 returns the versioned, digest-pinned snapshot the generator emitted
// and this module embeds. This is neocloud's read seam - the API ingests the
// artifact here rather than curating models itself. Clients pin Digest and poll
// Version.
func SnapshotV3() (CatalogSnapshot, error) {
	b, err := snapshotFS.ReadFile("snapshots/v3/catalog.json")
	if err != nil {
		return CatalogSnapshot{}, fmt.Errorf("read embedded snapshot: %w", err)
	}
	var snap CatalogSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return CatalogSnapshot{}, fmt.Errorf("parse embedded snapshot: %w", err)
	}
	return snap, nil
}
