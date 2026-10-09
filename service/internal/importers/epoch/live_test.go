package epoch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	catalog "github.com/fishman/neocloud-catalog"
)

// Epoch's live CSV still says what the recorded sample says for its rows, and
// merged with the committed overlay it gives the committed families. Opt in
// with NEOCLOUD_CATALOG_LIVE=1.
func TestLiveEpochMatchesRecordings(t *testing.T) {
	if os.Getenv("NEOCLOUD_CATALOG_LIVE") != "1" {
		t.Skip("NEOCLOUD_CATALOG_LIVE unset; skipping the live Epoch test")
	}
	overlayPath := filepath.Join(dataDir, "families.overlay.json")
	imp, err := New(CSV, overlayPath)
	if err != nil {
		t.Fatal(err)
	}
	body, err := imp.fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live, err := readEpoch(body)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := readEpoch(sample(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range recorded {
		if !reflect.DeepEqual(live[name], r) {
			t.Errorf("%s: live %+v, recorded %+v", name, live[name], r)
		}
	}

	overlay, err := readOverlay(overlayPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]catalog.Family{}
	for _, f := range committed(t).Families {
		want[f.Name] = f
	}
	for _, o := range overlay {
		v, err := merge(o, live)
		if err != nil {
			t.Errorf("%s: %v", o.Name, err)
			continue
		}
		b, err := json.Marshal(v.Columns)
		if err != nil {
			t.Fatal(err)
		}
		got := catalog.Family{Name: v.Key}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want[o.Name]) {
			t.Errorf("%s: live %+v, committed %+v", o.Name, got, want[o.Name])
		}
	}
}
