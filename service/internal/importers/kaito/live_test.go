package kaito

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"testing"
)

// KAITO's catalog at the pinned revision is byte for byte the recorded
// fixture. Opt in with NEOCLOUD_CATALOG_LIVE=1.
func TestLiveCatalogMatchesRecording(t *testing.T) {
	if os.Getenv("NEOCLOUD_CATALOG_LIVE") != "1" {
		t.Skip("NEOCLOUD_CATALOG_LIVE unset; skipping the live KAITO test")
	}
	imp := New(Catalog)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := imp.getter.Get(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if h := sha256.Sum256(body); hex.EncodeToString(h[:]) != fixtureSHA256 {
		t.Errorf("%s: sha256 %x, recorded %s", Catalog, h, fixtureSHA256)
	}
}
