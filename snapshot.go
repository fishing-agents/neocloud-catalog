// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed snapshots/v4/catalog.json
var snapshotJSON []byte

// SnapshotV4 returns the embedded generated snapshot.
func SnapshotV4() (CatalogSnapshot, error) {
	var snap CatalogSnapshot
	if err := json.Unmarshal(snapshotJSON, &snap); err != nil {
		return CatalogSnapshot{}, fmt.Errorf("parse embedded snapshot: %w", err)
	}
	return snap, nil
}
