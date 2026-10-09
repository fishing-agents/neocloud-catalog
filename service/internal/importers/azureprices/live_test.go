package azureprices

import (
	"os"
	"reflect"
	"testing"
)

// The recordings under testdata/azure still say what the Retail Prices API
// says: each committed Azure node type reads the same prices live, in the
// default regions, as recorded. Opt in with NEOCLOUD_CATALOG_LIVE=1.
func TestLivePricesMatchRecordings(t *testing.T) {
	if os.Getenv("NEOCLOUD_CATALOG_LIVE") != "1" {
		t.Skip("NEOCLOUD_CATALOG_LIVE unset; skipping the live Azure Retail Prices test")
	}
	recordedAPI := testImporter(newPricesAPI(t).URL, defaultRegions...)
	live := New(Endpoint, defaultRegions)
	for _, id := range []string{h100, mi300x} {
		got, err := live.read(ctx, id)
		if err != nil {
			t.Errorf("%s live: %v", id, err)
			continue
		}
		want, err := recordedAPI.read(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n live     %+v\n recorded %+v", id, got, want)
		}
	}
}
