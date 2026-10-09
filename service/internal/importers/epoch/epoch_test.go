package epoch

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const (
	dataDir   = "../../../../data"
	sampleCSV = "../../../testdata/epoch/ml_hardware.sample.csv"
	ccURL     = "https://developer.nvidia.com/cuda/gpus"
	vllmURL   = "https://github.com/vllm-project/vllm/blob/v0.31.0/vllm/platforms/cuda.py"
	h100URL   = "https://www.nvidia.com/en-us/data-center/h100/"
	l4URL     = "https://www.nvidia.com/en-us/data-center/l4/"
	adaURL    = "https://images.nvidia.com/aem-dam/Solutions/Data-Center/l4/nvidia-ada-gpu-architecture-whitepaper-V2.02.pdf"
)

const twoFamilies = `[
  {
    "name": "H100",
    "sku": "NVIDIA H100 SXM5 80 GB",
    "epoch_name": "NVIDIA H100 SXM5 80GB",
    "architecture": {"value": "hopper", "source": "` + h100URL + `"},
    "compute_capability": {"value": "9.0", "source": "` + ccURL + `"},
    "dtypes": {"value": ["fp16", "bf16", "fp8"], "source": "` + vllmURL + `"},
    "interconnect": {"value": "nvlink", "source": "` + h100URL + `"}
  },
  {
    "name": "L4",
    "sku": "NVIDIA L4 24 GB",
    "epoch_name": "NVIDIA L4",
    "architecture": {"value": "ada", "source": "` + l4URL + `"},
    "compute_capability": {"value": "8.9", "source": "` + ccURL + `"},
    "dtypes": {"value": ["fp16", "bf16", "fp8"], "source": "` + vllmURL + `"},
    "interconnect": {"value": "pcie", "source": "` + l4URL + `"},
    "interconnect_gbs": {"value": 64, "source": "` + l4URL + `"},
    "tflops": {"fp8": {"value": 242, "source": "` + adaURL + `"}}
  }
]`

var ctx = context.Background()

func sample(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(sampleCSV)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// epochServer serves csv as Epoch's ML hardware CSV.
func epochServer(t *testing.T, csv []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.Write(csv)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testImporter reads csv from a test server and the overlay from a file
// holding overlay, retrying quickly.
func testImporter(t *testing.T, csv []byte, overlay string) *Importer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "families.overlay.json")
	if err := os.WriteFile(path, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	i, err := New(epochServer(t, csv).URL+"/data/ml_hardware.csv", path)
	if err != nil {
		t.Fatal(err)
	}
	i.getter.Backoff = time.Millisecond
	return i
}

func newStore(t *testing.T, suffix string) (*store.Store, *gorm.DB) {
	t.Helper()
	sqlDB, gormDB := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	return store.New(gormDB), gormDB
}

// seed writes in into the upstream layer as a finished run of source.
func seed(t *testing.T, s *store.Store, source string, in store.Inputs) {
	t.Helper()
	run, err := s.StartImport(ctx, uuid.New(), "seed", model.LayerUpstream, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceLayer(ctx, model.LayerUpstream, run.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishImport(ctx, run.ID, "v1", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func committed(t *testing.T) store.Inputs {
	t.Helper()
	in, _, err := bootstrap.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// rowTimes maps every families row's id to its update time.
func rowTimes(t *testing.T, db *gorm.DB) map[uuid.UUID]time.Time {
	t.Helper()
	var rs []struct {
		ID        uuid.UUID
		UpdatedAt time.Time
	}
	if err := db.Table("families").Select("id, updated_at").Find(&rs).Error; err != nil {
		t.Fatal(err)
	}
	rows := map[uuid.UUID]time.Time{}
	for _, r := range rs {
		rows[r.ID] = r.UpdatedAt
	}
	return rows
}

func summaryOf(t *testing.T, run model.ImportRun) (importers.Summary, []string) {
	t.Helper()
	var s importers.Summary
	if err := json.Unmarshal(run.Summary, &s); err != nil {
		t.Fatalf("summary %s: %v", run.Summary, err)
	}
	conflicts := []string{}
	for _, c := range s.Conflicts {
		b, _ := json.Marshal(c)
		conflicts = append(conflicts, string(b))
	}
	return s, conflicts
}

func families(t *testing.T, s *store.Store) map[string]catalog.Family {
	t.Helper()
	eff, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fams := map[string]catalog.Family{}
	for _, f := range eff.Families {
		fams[f.Name] = f
	}
	return fams
}

func fields(t *testing.T, s *store.Store, family string) model.FieldProvenance {
	t.Helper()
	p, err := s.Provenance(ctx, store.KindFamily, family)
	if err != nil {
		t.Fatal(err)
	}
	return p[0].Fields
}

// Epoch's figures and the overlay's curated values merge into new families,
// Epoch's recorded as the run's, the overlay's as curation. A second run of
// the same input changes nothing.
func TestImportMergesEpochAndOverlay(t *testing.T) {
	s, db := newStore(t, "epoch_merge")
	imp := testImporter(t, sample(t), twoFamilies)
	first, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if first.Importer != "epoch" || first.Layer != model.LayerUpstream || first.Source != "epoch" || first.Status != model.RunSucceeded ||
		!strings.HasPrefix(first.InputVersion, "sha256:") {
		t.Fatalf("run = %+v, want a succeeded epoch run of the upstream layer", first)
	}
	sum, conflicts := summaryOf(t, first)
	if want := (store.Changes{"families": {Inserted: 2}}); !reflect.DeepEqual(sum.Changes, want) || len(conflicts) != 0 || len(sum.Errors) != 0 {
		t.Errorf("changes %+v conflicts %v errors %v, want %+v and nothing else", sum.Changes, conflicts, sum.Errors, want)
	}
	if sum.Sources == nil || *sum.Sources != (importers.SourceCounts{Read: 2}) {
		t.Errorf("sources = %+v, want both families read", sum.Sources)
	}

	fams := families(t, s)
	h100, l4 := fams["H100"], fams["L4"]
	if h100.Vendor != "nvidia" || h100.MemoryGB != 80 || h100.MemoryBandwidthGBs != 3350 {
		t.Errorf("H100 epoch facts = %s %v GB %v GB/s, want nvidia 80 3350", h100.Vendor, h100.MemoryGB, h100.MemoryBandwidthGBs)
	}
	if h100.ComputeCapability != "9.0" || h100.Architecture != "hopper" {
		t.Errorf("H100 overlay facts = %q %q", h100.ComputeCapability, h100.Architecture)
	}
	if !slices.Equal(h100.DTypes, []string{"fp16", "bf16", "fp8"}) {
		t.Errorf("H100 dtypes = %v, want the overlay's", h100.DTypes)
	}
	if h100.Interconnect != "nvlink" || h100.InterconnectGBs != 900 {
		t.Errorf("H100 interconnect = %s %v, want nvlink 900 (epoch intranode)", h100.Interconnect, h100.InterconnectGBs)
	}
	if want := map[string]float64{"fp16": 989.4, "bf16": 989.4, "fp8": 1979}; !maps.Equal(h100.TFLOPS, want) {
		t.Errorf("H100 tflops = %v, want %v", h100.TFLOPS, want)
	}
	for _, url := range []string{datasetURL, h100URL, ccURL, vllmURL} {
		if !slices.Contains(h100.Sources, url) {
			t.Errorf("H100 sources %v lack %s", h100.Sources, url)
		}
	}
	// Epoch leaves FP8 blank for L4: dtypes come from the overlay only, and the
	// overlay fills the missing figure.
	if !slices.Equal(l4.DTypes, []string{"fp16", "bf16", "fp8"}) {
		t.Errorf("L4 dtypes = %v", l4.DTypes)
	}
	if want := map[string]float64{"fp16": 121, "bf16": 121, "fp8": 242}; !maps.Equal(l4.TFLOPS, want) {
		t.Errorf("L4 tflops = %v, want %v", l4.TFLOPS, want)
	}
	if l4.Interconnect != "pcie" || l4.InterconnectGBs != 64 || l4.MemoryBandwidthGBs != 300.1 {
		t.Errorf("L4 = %s %v GB/s link, %v GB/s memory", l4.Interconnect, l4.InterconnectGBs, l4.MemoryBandwidthGBs)
	}

	fp := fields(t, s, "H100")
	for _, f := range []string{"vendor", "memory_gb", "memory_bandwidth_gbs", "interconnect_gbs"} {
		if p := fp[f]; p.Source != "epoch" || p.ImportRunID == nil || *p.ImportRunID != first.ID || p.SourceURL != imp.csvURL {
			t.Errorf("H100 %s provenance %+v, want the run's", f, p)
		}
	}
	for _, f := range []string{"architecture", "compute_capability", "dtypes", "interconnect", "sources"} {
		if p := fp[f]; p.Source != model.SourceCuration || p.SourceURL != imp.overlayURL || !strings.HasPrefix(p.SourceURL, "file:///") {
			t.Errorf("H100 %s provenance %+v, want curation from the overlay file", f, p)
		}
	}
	if p := fields(t, s, "L4")["tflops"]; p.Source != model.SourceCuration {
		t.Errorf("L4 tflops provenance %+v, want curation: the overlay gives its fp8", p)
	}

	atFirst := rowTimes(t, db)
	second, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if sum2, _ := summaryOf(t, second); len(sum2.Changes) != 0 || second.InputVersion != first.InputVersion {
		t.Errorf("second run changed %+v at input %s, want nothing at %s", sum2.Changes, second.InputVersion, first.InputVersion)
	}
	if !reflect.DeepEqual(rowTimes(t, db), atFirst) {
		t.Error("second run changed rows")
	}
}

// A family without its Epoch row or with a value lacking a source is listed
// and not imported; a run that imports no family fails.
func TestImportFailsOnMissingRowOrSource(t *testing.T) {
	for name, tc := range map[string]struct{ overlay, want string }{
		"unknown epoch row": {
			`[{"name": "X", "epoch_name": "NVIDIA X9000",
			  "architecture": {"value": "hopper", "source": "` + h100URL + `"},
			  "compute_capability": {"value": "9.0", "source": "` + ccURL + `"},
			  "dtypes": {"value": ["bf16"], "source": "` + vllmURL + `"}}]`,
			"NVIDIA X9000",
		},
		"no epoch row and no manual numbers": {
			`[{"name": "X", "vendor": {"value": "nvidia", "source": "` + h100URL + `"},
			  "architecture": {"value": "hopper", "source": "` + h100URL + `"},
			  "compute_capability": {"value": "9.0", "source": "` + ccURL + `"},
			  "dtypes": {"value": ["bf16"], "source": "` + vllmURL + `"}}]`,
			"memory_gb",
		},
		"value without source": {
			`[{"name": "H100", "epoch_name": "NVIDIA H100 SXM5 80GB",
			  "architecture": {"value": "hopper"},
			  "compute_capability": {"value": "9.0", "source": "` + ccURL + `"},
			  "dtypes": {"value": ["bf16"], "source": "` + vllmURL + `"}}]`,
			"architecture",
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, db := newStore(t, "epoch_fail")
			run, err := importers.Execute(ctx, s, testImporter(t, sample(t), tc.overlay), uuid.New())
			if err == nil || run.Status != model.RunFailed || !strings.Contains(run.Error, "no family could be imported") ||
				!strings.Contains(run.Error, tc.want) {
				t.Fatalf("run %+v, err %v; want failed naming %q", run, err, tc.want)
			}
			if n := len(rowTimes(t, db)); n != 0 {
				t.Errorf("failed run wrote %d families", n)
			}
		})
	}
}

// Over the bootstrapped committed catalog the committed overlay imports what
// data/families.json holds: families whose Epoch row the sample has change
// nothing, keeping the bootstrap's provenance, the others are listed and keep
// their rows.
func TestImportCommittedFamilies(t *testing.T) {
	s, db := newStore(t, "epoch_committed")
	in := committed(t)
	boot, err := bootstrap.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importers.Execute(ctx, s, boot, uuid.New()); err != nil {
		t.Fatal(err)
	}
	before := rowTimes(t, db)
	overlay, err := os.ReadFile(filepath.Join(dataDir, "families.overlay.json"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := importers.Execute(ctx, s, testImporter(t, sample(t), string(overlay)), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	sum, conflicts := summaryOf(t, run)
	if len(sum.Changes) != 0 || len(conflicts) != 0 {
		t.Errorf("changes %+v conflicts %v, want none", sum.Changes, conflicts)
	}
	want := []string{}
	for _, f := range []struct{ family, row string }{
		{"A10G", "NVIDIA A10G"}, {"RTX-4090", "NVIDIA GeForce RTX 4090"}, {"A100", "NVIDIA A100 SXM4 80 GB"},
		{"H200", "NVIDIA H200 SXM"}, {"MI300X", "AMD Instinct MI300X"},
	} {
		want = append(want, f.family+`: no Epoch row "`+f.row+`"`)
	}
	if !slices.Equal(sum.Errors, want) {
		t.Errorf("errors =\n%s\nwant\n%s", strings.Join(sum.Errors, "\n"), strings.Join(want, "\n"))
	}
	if sum.Sources == nil || *sum.Sources != (importers.SourceCounts{Read: 2, Failed: 5}) {
		t.Errorf("sources = %+v, want H100 and L4 read, 5 failed", sum.Sources)
	}
	if !reflect.DeepEqual(rowTimes(t, db), before) {
		t.Error("the import changed rows")
	}
	got := families(t, s)
	for _, f := range in.Families {
		if !reflect.DeepEqual(got[f.Name], f) {
			t.Errorf("%s = %+v, want the committed %+v", f.Name, got[f.Name], f)
		}
	}
	if p := fields(t, s, "H100")["memory_gb"]; p.Source != "bootstrap-from-json" {
		t.Errorf("H100 memory_gb provenance %+v, want the equal bootstrapped value's kept", p)
	}
}

// A fresh store gets the families in name order, the order of
// data/families.json, whatever the overlay's order (L4 before H100).
func TestFreshImportOrdersByName(t *testing.T) {
	s, _ := newStore(t, "epoch_order")
	overlay, err := os.ReadFile(filepath.Join(dataDir, "families.overlay.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importers.Execute(ctx, s, testImporter(t, sample(t), string(overlay)), uuid.New()); err != nil {
		t.Fatal(err)
	}
	eff, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got, want []string
	for _, f := range eff.Families {
		got = append(got, f.Name)
	}
	for _, f := range committed(t).Families {
		if f.Name == "H100" || f.Name == "L4" {
			want = append(want, f.Name)
		}
	}
	if !slices.Equal(got, want) || !slices.Equal(got, []string{"H100", "L4"}) {
		t.Errorf("families %v, want %v as in data/families.json", got, want)
	}
}

// Epoch's figures keep curated values they contradict, listed (R12); the
// overlay's values are curation and replace them. A family the overlay does
// not list is left alone.
func TestImportKeepsCuratedValues(t *testing.T) {
	s, _ := newStore(t, "epoch_keep")
	in := committed(t)
	byName := map[string]*catalog.Family{}
	for i := range in.Families {
		byName[in.Families[i].Name] = &in.Families[i]
	}
	byName["H100"].MemoryBandwidthGBs = 3000
	byName["H100"].Architecture = "ampere"
	seed(t, s, model.SourceCuration, in)
	run, err := importers.Execute(ctx, s, testImporter(t, sample(t), twoFamilies), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	sum, conflicts := summaryOf(t, run)
	if want := []string{`{"kind":"family","key":"H100","field":"memory_bandwidth_gbs","curated":3000,"imported":3350}`}; !slices.Equal(conflicts, want) {
		t.Errorf("conflicts %v, want %v", conflicts, want)
	}
	if want := (store.Changes{"families": {Updated: 1}}); !reflect.DeepEqual(sum.Changes, want) {
		t.Errorf("changes %+v, want %+v", sum.Changes, want)
	}
	got := families(t, s)
	if h := got["H100"]; h.MemoryBandwidthGBs != 3000 || h.Architecture != "hopper" {
		t.Errorf("H100 bandwidth %v architecture %s, want the curated 3000 and the overlay's hopper", h.MemoryBandwidthGBs, h.Architecture)
	}
	fp := fields(t, s, "H100")
	if p := fp["memory_bandwidth_gbs"]; p.Source != model.SourceCuration || p.ImportRunID == nil || *p.ImportRunID == run.ID {
		t.Errorf("H100 bandwidth provenance %+v, want the seeded curation kept", p)
	}
	if p := fp["architecture"]; p.Source != model.SourceCuration || p.ImportRunID == nil || *p.ImportRunID != run.ID {
		t.Errorf("H100 architecture provenance %+v, want curation by run %s", p, run.ID)
	}
	if !reflect.DeepEqual(got["A100"], *byName["A100"]) {
		t.Errorf("A100 = %+v, want untouched", got["A100"])
	}
}

// Over bootstrapped values (R21) the import writes Epoch's figures whole and
// the overlay's values: a TFLOPS entry Epoch no longer gives is gone, each
// column records its origin, nothing is listed as a conflict.
func TestImportReplacesImportedValues(t *testing.T) {
	s, _ := newStore(t, "epoch_replace")
	in := committed(t)
	for i := range in.Families {
		if f := &in.Families[i]; f.Name == "H100" {
			f.TFLOPS = map[string]float64{"fp16": 1, "bf16": 1, "fp8": 1, "fp4": 1}
			f.MemoryGB = 1
			f.Architecture = "ampere"
		}
	}
	seed(t, s, "bootstrap-from-json", in)
	imp := testImporter(t, sample(t), twoFamilies)
	run, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	sum, conflicts := summaryOf(t, run)
	if want := (store.Changes{"families": {Updated: 1}}); !reflect.DeepEqual(sum.Changes, want) || len(conflicts) != 0 {
		t.Errorf("changes %+v conflicts %v, want %+v and none", sum.Changes, conflicts, want)
	}
	h := families(t, s)["H100"]
	if want := map[string]float64{"fp16": 989.4, "bf16": 989.4, "fp8": 1979}; !maps.Equal(h.TFLOPS, want) || h.MemoryGB != 80 || h.Architecture != "hopper" {
		t.Errorf("H100 tflops %v memory %v architecture %s, want Epoch's %v and 80, the overlay's hopper", h.TFLOPS, h.MemoryGB, h.Architecture, want)
	}
	fp := fields(t, s, "H100")
	for _, f := range []string{"tflops", "memory_gb"} {
		if p := fp[f]; p.Source != "epoch" || p.ImportRunID == nil || *p.ImportRunID != run.ID {
			t.Errorf("H100 %s provenance %+v, want run %s", f, p, run.ID)
		}
	}
	if p := fp["architecture"]; p.Source != model.SourceCuration || p.SourceURL != imp.overlayURL || *p.ImportRunID != run.ID {
		t.Errorf("H100 architecture provenance %+v, want curation from the overlay by run %s", p, run.ID)
	}
	if p := fp["vendor"]; p.Source != "bootstrap-from-json" {
		t.Errorf("H100 vendor provenance %+v, want the unchanged value's", p)
	}
}

// A correction the overlay withdraws gives way to Epoch's figure: curation
// from the overlay file is the overlay's to take back, no conflict.
func TestImportWithdrawnCorrection(t *testing.T) {
	s, _ := newStore(t, "epoch_withdraw")
	corrected := strings.Replace(twoFamilies, `"epoch_name": "NVIDIA H100 SXM5 80GB",`,
		`"epoch_name": "NVIDIA H100 SXM5 80GB", "memory_bandwidth_gbs": {"value": 3000, "source": "`+h100URL+`"},`, 1)
	imp := testImporter(t, sample(t), corrected)
	if _, err := importers.Execute(ctx, s, imp, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if h := families(t, s)["H100"]; h.MemoryBandwidthGBs != 3000 {
		t.Fatalf("H100 bandwidth %v, want the overlay's 3000", h.MemoryBandwidthGBs)
	}
	if err := os.WriteFile(imp.overlay, []byte(twoFamilies), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, conflicts := summaryOf(t, run); len(conflicts) != 0 {
		t.Errorf("conflicts %v, want none", conflicts)
	}
	if h := families(t, s)["H100"]; h.MemoryBandwidthGBs != 3350 {
		t.Errorf("H100 bandwidth %v, want Epoch's 3350", h.MemoryBandwidthGBs)
	}
	if p := fields(t, s, "H100")["memory_bandwidth_gbs"]; p.Source != "epoch" || *p.ImportRunID != run.ID {
		t.Errorf("H100 bandwidth provenance %+v, want run %s", p, run.ID)
	}
}

// The input version follows the facts a run used: an Epoch row no family uses
// leaves it, a used figure changes it.
func TestInputVersionFollowsTheData(t *testing.T) {
	s, _ := newStore(t, "epoch_version")
	version := func(csv string) string {
		t.Helper()
		run, err := importers.Execute(ctx, s, testImporter(t, []byte(csv), twoFamilies), uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		return run.InputVersion
	}
	csv := string(sample(t))
	recorded := version(csv)
	if v := version(csv + "NVIDIA X9000,NVIDIA,1,,,1,1,\n"); v != recorded {
		t.Errorf("an unused row changed the input version: %s, recorded %s", v, recorded)
	}
	if v := version(strings.Replace(csv, "3350000000000.0", "3360000000000.0", 1)); v == recorded {
		t.Error("a changed H100 bandwidth kept the input version")
	}
}

// 429 and 5xx are retried; a Retry-After beyond maxRetryAfter fails at once.
func TestFetchRetries(t *testing.T) {
	var calls atomic.Int32
	statuses := []int{}
	retryAfter := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(calls.Add(1)) - 1
		if n < len(statuses) {
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(statuses[n])
			return
		}
		w.Write([]byte("csv"))
	}))
	t.Cleanup(srv.Close)
	fetch := func() ([]byte, error) {
		calls.Store(0)
		i, err := New(srv.URL, "overlay.json")
		if err != nil {
			t.Fatal(err)
		}
		i.getter.Backoff = time.Millisecond
		return i.fetch(ctx)
	}
	statuses = []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}
	if b, err := fetch(); err != nil || string(b) != "csv" || calls.Load() != 3 {
		t.Errorf("503 then 429: %d requests, body %q, err %v; want 3 and the body", calls.Load(), b, err)
	}
	statuses = []int{http.StatusNotFound}
	if _, err := fetch(); err == nil || !strings.Contains(err.Error(), "404") || calls.Load() != 1 {
		t.Errorf("404: %d requests, err %v; want 1 and the status", calls.Load(), err)
	}
	statuses, retryAfter = []int{http.StatusTooManyRequests}, "3600"
	start := time.Now()
	if _, err := fetch(); err == nil || !strings.Contains(err.Error(), "Retry-After 1h0m0s exceeds 5m0s") || calls.Load() != 1 ||
		time.Since(start) > time.Second {
		t.Errorf("Retry-After 3600: %d requests, err %v; want an immediate error naming it", calls.Load(), err)
	}
}
