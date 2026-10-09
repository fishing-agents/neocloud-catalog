package azureprices

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const (
	dataDir    = "../../../../data"
	fixtureDir = "../../../testdata/azure"
	h100       = "azure/Standard_ND96isr_H100_v5"
	mi300x     = "azure/Standard_ND96isr_MI300X_v5"
	// h100Query is the query the H100 fixture was recorded with.
	h100Query = "%24filter=serviceName+eq+%27Virtual+Machines%27+and+armSkuName+eq+%27Standard_ND96isr_H100_v5%27+and+%28armRegionName+eq+%27eastus%27+or+armRegionName+eq+%27eastus2%27%29+and+isPrimaryMeterRegion+eq+true+and+contains%28productName%2C+%27Windows%27%29+eq+false+and+%28priceType+eq+%27Consumption%27+or+reservationTerm+eq+%271+Year%27%29"
)

var (
	ctx            = context.Background()
	defaultRegions = []string{"eastus", "eastus2"}
	skuOf          = regexp.MustCompile(`armSkuName eq '([^']*)'`)
)

// recorded are the prices the fixtures give for the default regions: the
// 1-year reservations are the term's price over 8760 hours, to the
// micro-dollar (551221 and 269107 USD a year).
var recorded = map[string]float64{
	h100 + " eastus on_demand":      98.32,
	h100 + " eastus spot":           18.169536,
	h100 + " eastus reserved_1y":    62.924772,
	h100 + " eastus2 on_demand":     98.32,
	h100 + " eastus2 spot":          18.169536,
	h100 + " eastus2 reserved_1y":   62.924772,
	mi300x + " eastus2 on_demand":   48,
	mi300x + " eastus2 spot":        8.8704,
	mi300x + " eastus2 reserved_1y": 30.719977,
}

// pricesAPI serves the responses recorded under testdata/azure: the file
// <ARM SKU name>.200.json answers the query of that SKU. fail names SKUs
// answered 503 instead. It records the queries it got.
type pricesAPI struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
}

func newPricesAPI(t *testing.T, fail ...string) *pricesAPI {
	t.Helper()
	api := &pricesAPI{}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.queries = append(api.queries, r.URL.RawQuery)
		api.mu.Unlock()
		m := skuOf.FindStringSubmatch(r.URL.Query().Get("$filter"))
		if m == nil || r.URL.Path != "/api/retail/prices" {
			t.Errorf("unrecorded request %s", r.URL)
			http.NotFound(w, r)
			return
		}
		for _, f := range fail {
			if "azure/"+m[1] == f {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		b, err := os.ReadFile(filepath.Join(fixtureDir, m[1]+".200.json"))
		if err != nil {
			t.Errorf("unrecorded SKU %s", m[1])
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	t.Cleanup(api.Close)
	return api
}

// testImporter reads endpoint in regions, retrying quickly.
func testImporter(endpoint string, regions ...string) *Importer {
	i := New(endpoint+"/api/retail/prices", regions)
	i.getter.Backoff = time.Millisecond
	return i
}

// newStore is a store holding the committed catalog in upstream, as the
// bootstrap writes it.
func newStore(t *testing.T, suffix string) (*store.Store, *gorm.DB) {
	t.Helper()
	sqlDB, db := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	s := store.New(db)
	in, _, err := bootstrap.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartImport(ctx, uuid.New(), "bootstrap-from-json", model.LayerUpstream, "bootstrap-from-json", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, run.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishImport(ctx, run.ID, "v1", nil, nil); err != nil {
		t.Fatal(err)
	}
	return s, db
}

func priceKey(p model.NodeTypePrice) string { return p.NodeTypeID + " " + p.Region + " " + p.Billing }

// prices are the rows of layer by node type, region and billing.
func prices(t *testing.T, db *gorm.DB, layer string) map[string]model.NodeTypePrice {
	t.Helper()
	var rows []model.NodeTypePrice
	if err := db.Where("layer = ?", layer).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[string]model.NodeTypePrice{}
	for _, r := range rows {
		out[priceKey(r)] = r
	}
	return out
}

func usd(rows map[string]model.NodeTypePrice, prefix string) map[string]float64 {
	out := map[string]float64{}
	for k, r := range rows {
		if strings.HasPrefix(k, prefix) {
			out[k] = r.USDPerHour
		}
	}
	return out
}

func summaryOf(t *testing.T, run model.ImportRun) importers.Summary {
	t.Helper()
	var s importers.Summary
	if err := json.Unmarshal(run.Summary, &s); err != nil {
		t.Fatalf("summary %s: %v", run.Summary, err)
	}
	return s
}

func price(layer, nodeType, region, billing string, usd float64) *model.NodeTypePrice {
	return &model.NodeTypePrice{Provenance: model.Provenance{Layer: layer, Source: "seed", AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		NodeTypeID: nodeType, Region: region, Billing: billing, USDPerHour: usd}
}

// The recorded responses give on-demand, spot and 1-year reserved prices of
// both Azure node types, stamped with the run, as of its start. The run
// replaces the upstream Azure prices only: other providers' and local prices
// stay. A second run of the same input changes nothing.
func TestImportRecordedPrices(t *testing.T) {
	s, db := newStore(t, "azure_recorded")
	for _, p := range []*model.NodeTypePrice{
		price(model.LayerUpstream, "aws/p5.48xlarge", "us-east-1", model.BillingOnDemand, 55.04),
		price(model.LayerUpstream, h100, "westeurope", model.BillingOnDemand, 110),
		price(model.LayerUpstream, "azure/Standard_Gone", "eastus", model.BillingSpot, 1),
		price(model.LayerLocal, h100, "eastus", model.BillingOnDemand, 90),
	} {
		if err := db.Create(p).Error; err != nil {
			t.Fatal(err)
		}
	}
	before := prices(t, db, model.LayerLocal)
	api := newPricesAPI(t)
	imp := testImporter(api.URL, defaultRegions...)
	first, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if first.Importer != "azure-prices" || first.Source != "azure-prices" || first.SourceURL != api.URL+"/api/retail/prices" ||
		first.Layer != model.LayerUpstream || first.Status != model.RunSucceeded || !strings.HasPrefix(first.InputVersion, "sha256:") {
		t.Fatalf("run = %+v, want a succeeded azure-prices run of the upstream layer", first)
	}
	sum := summaryOf(t, first)
	if want := (store.Changes{"node_type_prices": {Inserted: 9, Deleted: 2}}); !reflect.DeepEqual(sum.Changes, want) || len(sum.Errors) != 0 ||
		sum.Sources == nil || *sum.Sources != (importers.SourceCounts{Read: 2}) {
		t.Errorf("summary %+v, want %+v from both node types", sum, want)
	}
	if !strings.Contains(strings.Join(api.queries, "\n"), h100Query) {
		t.Errorf("queries %q, want the recorded H100 query", api.queries)
	}

	up := prices(t, db, model.LayerUpstream)
	if got := usd(up, "azure/"); !reflect.DeepEqual(got, recorded) {
		t.Errorf("upstream Azure prices =\n%v\nwant\n%v", got, recorded)
	}
	for k, p := range up {
		if strings.HasPrefix(k, "azure/") && (p.Source != "azure-prices" || p.SourceURL != first.SourceURL ||
			!p.AsOf.Equal(first.StartedAt) || p.ImportRunID == nil || *p.ImportRunID != first.ID) {
			t.Errorf("%s provenance %+v, want the run's as of its start", k, p.Provenance)
		}
	}
	if p, ok := up["aws/p5.48xlarge us-east-1 on_demand"]; !ok || p.Source != "seed" || p.USDPerHour != 55.04 {
		t.Errorf("AWS price %+v, want it untouched", p)
	}
	if after := prices(t, db, model.LayerLocal); !reflect.DeepEqual(after, before) {
		t.Errorf("local prices %+v, want them untouched", after)
	}

	second, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if sum := summaryOf(t, second); len(sum.Changes) != 0 || second.InputVersion != first.InputVersion {
		t.Errorf("second run changed %+v, input version %s (first %s)", sum.Changes, second.InputVersion, first.InputVersion)
	}
	if again := prices(t, db, model.LayerUpstream); !reflect.DeepEqual(again, up) {
		t.Error("second run rewrote prices")
	}
}

// A node type priced in none of the regions is listed as a failed source and
// loses its prices (R23); a node type read replaces its prices of every
// region, so regions no longer configured lose theirs.
func TestUnpricedNodeTypeFailsItsSource(t *testing.T) {
	s, db := newStore(t, "azure_unpriced")
	api := newPricesAPI(t)
	if _, err := importers.Execute(ctx, s, testImporter(api.URL, defaultRegions...), uuid.New()); err != nil {
		t.Fatal(err)
	}
	run, err := importers.Execute(ctx, s, testImporter(api.URL, "eastus"), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	sum := summaryOf(t, run)
	wantErrors := []string{mi300x + ": no Linux on-demand, spot or 1-year reserved price in eastus"}
	if run.Status != model.RunSucceeded || !reflect.DeepEqual(sum.Errors, wantErrors) || *sum.Sources != (importers.SourceCounts{Read: 1, Failed: 1}) ||
		!reflect.DeepEqual(sum.Changes, store.Changes{"node_type_prices": {Deleted: 6}}) {
		t.Errorf("run %s summary %+v, want errors %q and the eastus2 prices deleted", run.Status, sum, wantErrors)
	}
	want := map[string]float64{}
	for k, v := range recorded {
		if strings.HasPrefix(k, h100+" eastus ") {
			want[k] = v
		}
	}
	if got := usd(prices(t, db, model.LayerUpstream), "azure/"); !reflect.DeepEqual(got, want) {
		t.Errorf("upstream prices =\n%v\nwant\n%v", got, want)
	}
}

// A node type whose prices cannot be read keeps them and is listed; a run
// that reads no node type fails and writes nothing.
func TestUnreadablePrices(t *testing.T) {
	s, db := newStore(t, "azure_unreadable")
	if _, err := importers.Execute(ctx, s, testImporter(newPricesAPI(t).URL, defaultRegions...), uuid.New()); err != nil {
		t.Fatal(err)
	}
	before := prices(t, db, model.LayerUpstream)
	run, err := importers.Execute(ctx, s, testImporter(newPricesAPI(t, mi300x).URL, defaultRegions...), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if sum := summaryOf(t, run); len(sum.Changes) != 0 || len(sum.Errors) != 1 || !strings.HasPrefix(sum.Errors[0], mi300x+": GET ") ||
		!strings.Contains(sum.Errors[0], "503") || *sum.Sources != (importers.SourceCounts{Read: 1, Failed: 1}) {
		t.Errorf("summary %+v, want MI300X listed and nothing changed", sum)
	}
	run, err = importers.Execute(ctx, s, testImporter(newPricesAPI(t, h100, mi300x).URL, defaultRegions...), uuid.New())
	if err == nil || run.Status != model.RunFailed || !strings.Contains(run.Error, "no node type's prices could be read") {
		t.Errorf("run %s (%s), err %v, want a failed run", run.Status, run.Error, err)
	}
	if after := prices(t, db, model.LayerUpstream); !reflect.DeepEqual(after, before) {
		t.Error("an unreadable run changed prices")
	}
}

// A catalog without Azure node types reads nothing and is no failure: the run
// succeeds and the upstream Azure prices go.
func TestNoAzureNodeTypes(t *testing.T) {
	sqlDB, db := dbtest.Fresh(t, "azure_none")
	dbtest.Migrate(t, sqlDB)
	for _, p := range []*model.NodeTypePrice{
		price(model.LayerUpstream, "aws/p5.48xlarge", "us-east-1", model.BillingOnDemand, 55.04),
		price(model.LayerUpstream, h100, "eastus", model.BillingOnDemand, 98.32),
	} {
		if err := db.Create(p).Error; err != nil {
			t.Fatal(err)
		}
	}
	api := newPricesAPI(t)
	run, err := importers.Execute(ctx, store.New(db), testImporter(api.URL, defaultRegions...), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if sum := summaryOf(t, run); run.Status != model.RunSucceeded || len(api.queries) != 0 || *sum.Sources != (importers.SourceCounts{}) ||
		!reflect.DeepEqual(sum.Changes, store.Changes{"node_type_prices": {Deleted: 1}}) {
		t.Errorf("run %s summary %+v after %d queries, want a succeeded run deleting the Azure price", run.Status, sum, len(api.queries))
	}
	if got := usd(prices(t, db, model.LayerUpstream), ""); !reflect.DeepEqual(got, map[string]float64{"aws/p5.48xlarge us-east-1 on_demand": 55.04}) {
		t.Errorf("upstream prices %v, want the AWS price only", got)
	}
}

// Rows the importer does not price are skipped, zero prices among them; a
// node type left without a row has no prices. Prices it cannot take as USD
// per hour, or two prices of one region and billing, fail the node type.
func TestPricesOfItems(t *testing.T) {
	imp := New(Endpoint, defaultRegions)
	row := func(region, sku, typ, term string, usd float64) item {
		return item{ArmSkuName: "S", ArmRegionName: region, SkuName: sku, ProductName: "Virtual Machines S Series", Type: typ,
			ReservationTerm: term, UnitOfMeasure: "1 Hour", CurrencyCode: "USD", RetailPrice: usd, IsPrimaryMeterRegion: true}
	}
	windows := row("eastus", "S", "Consumption", "", 9)
	windows.ProductName += " Windows"
	secondary := row("eastus", "S", "Consumption", "", 9)
	secondary.IsPrimaryMeterRegion = false
	got, err := imp.prices("azure/S", []item{
		row("eastus", "S", "Consumption", "", 2.5), row("eastus", "S Spot", "Consumption", "", 0.5),
		row("eastus", "S Low Priority", "Consumption", "", 0.4), row("eastus", "S", "Reservation", "1 Year", 8760),
		row("eastus", "S", "Reservation", "3 Years", 1), row("eastus", "S", "DevTestConsumption", "", 1),
		row("westus", "S", "Consumption", "", 1), windows, secondary, row("eastus2", "S", "Consumption", "", 2.5),
		row("eastus2", "S Spot", "Consumption", "", 0), row("eastus2", "S", "Consumption", "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"eastus on_demand 2.5", "eastus reserved_1y 1", "eastus spot 0.5", "eastus2 on_demand 2.5"}
	var have []string
	for _, p := range got {
		have = append(have, strings.TrimPrefix(priceKey(p), "azure/S ")+" "+strconv.FormatFloat(p.USDPerHour, 'f', -1, 64))
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("prices %q, want %q", have, want)
	}

	euro := row("eastus", "S", "Consumption", "", 2)
	euro.CurrencyCode = "EUR"
	monthly := row("eastus", "S", "Consumption", "", 2)
	monthly.UnitOfMeasure = "1/Month"
	for name, c := range map[string]struct {
		items []item
		want  string
	}{
		"two prices": {[]item{row("eastus", "S", "Consumption", "", 2), row("eastus", "S", "Consumption", "", 3)}, "eastus on_demand: prices 2 and 3"},
		"currency":   {[]item{euro}, "eastus on_demand: 2 EUR per 1 Hour"},
		"unit":       {[]item{monthly}, "eastus on_demand: 2 USD per 1/Month"},
	} {
		if _, err := imp.prices("azure/S", c.items); err == nil || err.Error() != c.want {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
	if got, err := imp.prices("azure/S", []item{windows, row("eastus", "S", "Consumption", "", 0)}); len(got) != 0 || err != nil {
		t.Errorf("no priced row: %+v, %v; want no prices and no error", got, err)
	}
}

// Throttling is retried after Retry-After, unless it asks for longer than the
// importer waits.
func TestRetryAfter(t *testing.T) {
	var calls atomic.Int32
	retryAfter := "0"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"Items":[]}`))
	}))
	t.Cleanup(srv.Close)
	imp := testImporter(srv.URL, "eastus")
	if _, err := imp.items(ctx, srv.URL+"/api/retail/prices"); err != nil || calls.Load() != 2 {
		t.Errorf("items after a 429 = %v, %d calls, want success on the second", err, calls.Load())
	}
	calls.Store(0)
	retryAfter = "3600"
	start := time.Now()
	if _, err := imp.items(ctx, srv.URL+"/api/retail/prices"); err == nil || !strings.Contains(err.Error(), "Retry-After") || calls.Load() != 1 ||
		time.Since(start) > 10*time.Second {
		t.Errorf("items after Retry-After 3600 = %v, %d calls, want a failure without waiting", err, calls.Load())
	}
}

// A response continues at its NextPageLink on the API's origin only.
func TestNextPageLink(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	var next string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$skip") == "" {
			json.NewEncoder(w).Encode(map[string]any{"Items": []item{{ArmSkuName: "a"}}, "NextPageLink": next})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Items": []item{{ArmSkuName: "b"}}, "NextPageLink": nil})
	}))
	t.Cleanup(srv.Close)
	imp := testImporter(srv.URL, "eastus")
	next = srv.URL + "/api/retail/prices?$skip=1000"
	items, err := imp.items(ctx, srv.URL+"/api/retail/prices")
	if err != nil || len(items) != 2 || items[1].ArmSkuName != "b" {
		t.Errorf("items = %+v, %v, want both pages", items, err)
	}
	next = other.URL + "/api/retail/prices?$skip=1000"
	if _, err := imp.items(ctx, srv.URL+"/api/retail/prices"); err == nil || elsewhere.Load() != 0 {
		t.Errorf("items followed a link elsewhere: %v, %d calls there", err, elsewhere.Load())
	}
}
