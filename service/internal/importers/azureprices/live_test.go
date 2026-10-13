// SPDX-License-Identifier: Apache-2.0

package azureprices

import (
	"os"
	"reflect"
	"testing"
)

// Live prices match testdata/azure. Opt in with NEOCLOUD_CATALOG_LIVE=1.
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
