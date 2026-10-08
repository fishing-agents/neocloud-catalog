package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed snapshots/v4/catalog.json
var snapshotJSON []byte

// SnapshotV4 returns the versioned, digest-pinned snapshot the generator emitted
// and this module embeds. This is neocloud's read seam - the API ingests the
// artifact here rather than curating models itself. Clients pin Digest and poll
// Version.
func SnapshotV4() (CatalogSnapshot, error) {
	var snap CatalogSnapshot
	if err := json.Unmarshal(snapshotJSON, &snap); err != nil {
		return CatalogSnapshot{}, fmt.Errorf("parse embedded snapshot: %w", err)
	}
	return snap, nil
}
