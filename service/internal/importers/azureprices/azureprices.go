// Package azureprices imports Azure Retail Prices into the upstream layer: for
// each Azure node type of the effective catalog (azure/<ARM SKU name>), in
// each configured region, the Linux virtual machine's on-demand, spot and
// 1-year reserved price in USD per hour.
//
// A run replaces the upstream prices of Azure node types, in every region,
// with what it read; other providers' prices and the local layer stay. A node
// type whose prices cannot be read, or that has no price in any configured
// region, is listed in the run's errors and keeps its prices; the run fails
// when it reads no node type, when its write fails or when it is cancelled,
// which writes nothing. Its input version hashes the prices it read and its
// errors.
package azureprices

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const (
	// Endpoint is the Azure Retail Prices API.
	Endpoint = "https://prices.azure.com/api/retail/prices"
	// Prefix starts the id of an Azure node type, followed by its ARM SKU
	// name.
	Prefix = "azure/"
	// maxBody bounds a response; a page holds at most 1000 rows of about
	// 600 bytes.
	maxBody = 4 << 20
	// maxPages bounds the pages of one node type's prices, a few rows.
	maxPages = 20
	// maxRetryAfter bounds a Retry-After the importer waits out; the API
	// asking for longer fails the node type rather than spending
	// jobs.job_timeout.
	maxRetryAfter = 5 * time.Minute
	// hoursPerYear converts a 1-year reservation's price to a price per
	// hour: the API gives the price of the whole term although its
	// unitOfMeasure says "1 Hour", and Azure's year is 365 x 24 hours.
	hoursPerYear = 365 * 24
)

// sku is the form of an ARM SKU name; it keeps the OData filter well formed.
var sku = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// Importer reads the Retail Prices API at endpoint in regions.
type Importer struct {
	endpoint string
	regions  []string
	client   *http.Client
	backoff  time.Duration
	retries  int
}

// New returns the importer of the API at endpoint in regions, ARM region
// names (the config validates them).
func New(endpoint string, regions []string) *Importer {
	return &Importer{endpoint: endpoint, regions: regions, client: &http.Client{Timeout: time.Minute}, backoff: time.Second, retries: 3}
}

func (*Importer) Name() string { return "azure-prices" }

func (i *Importer) Origin() importers.Origin {
	return importers.Origin{Layer: model.LayerUpstream, Source: "azure-prices", SourceURL: i.endpoint}
}

func (i *Importer) Run(ctx context.Context, s *store.Store, run uuid.UUID) (importers.Summary, error) {
	in, err := s.Effective(ctx)
	if err != nil {
		return importers.Summary{}, err
	}
	var ids []string
	for _, nt := range in.NodeTypes {
		if strings.HasPrefix(nt.ID, Prefix) {
			ids = append(ids, nt.ID)
		}
	}
	slices.Sort(ids)
	sum := importers.Summary{Sources: &importers.SourceCounts{}}
	p := store.Prices{Prefix: Prefix}
	for _, id := range ids {
		rows, err := i.read(ctx, id)
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		if err != nil {
			sum.Errors = append(sum.Errors, id+": "+err.Error())
			sum.Sources.Failed++
			p.Kept = append(p.Kept, id)
			continue
		}
		sum.Sources.Read++
		p.Rows = append(p.Rows, rows...)
	}
	input, err := json.Marshal(struct {
		Prices []model.NodeTypePrice
		Errors []string
	}{p.Rows, sum.Errors})
	if err != nil {
		return importers.Summary{}, err
	}
	h := sha256.Sum256(input)
	sum.InputVersion = "sha256:" + hex.EncodeToString(h[:])
	if sum.Sources.Read == 0 {
		return sum, fmt.Errorf("no node type's prices could be read (Azure node types %q): %s", ids, strings.Join(sum.Errors, "; "))
	}
	sum.Changes, _, err = s.Patch(ctx, run, store.Patch{Prices: &p})
	return sum, err
}

// read returns the prices of the Azure node type id, sorted by region and
// billing.
func (i *Importer) read(ctx context.Context, id string) ([]model.NodeTypePrice, error) {
	name := strings.TrimPrefix(id, Prefix)
	if !sku.MatchString(name) {
		return nil, fmt.Errorf("%q is not an ARM SKU name", name)
	}
	regions := make([]string, len(i.regions))
	for j, r := range i.regions {
		regions[j] = "armRegionName eq '" + r + "'"
	}
	filter := "serviceName eq 'Virtual Machines' and armSkuName eq '" + name + "' and (" + strings.Join(regions, " or ") +
		") and isPrimaryMeterRegion eq true and contains(productName, 'Windows') eq false and (priceType eq 'Consumption' or reservationTerm eq '1 Year')"
	items, err := i.items(ctx, i.endpoint+"?"+url.Values{"$filter": {filter}}.Encode())
	if err != nil {
		return nil, err
	}
	return i.prices(id, items)
}

// item is the part of a Retail Prices API row the importer reads.
type item struct {
	ArmSkuName, ArmRegionName, SkuName, ProductName    string
	Type, ReservationTerm, UnitOfMeasure, CurrencyCode string
	RetailPrice                                        float64
	IsPrimaryMeterRegion                               bool
}

// prices takes the prices of node type id from its rows: a Linux primary
// meter's consumption price is on demand, or spot when its SKU is a Spot one
// (Low Priority is not priced); a 1-year reservation's is reserved_1y. Two
// prices of one region and billing, or a price not in USD per hour, fail it.
func (i *Importer) prices(id string, items []item) ([]model.NodeTypePrice, error) {
	usd := map[[2]string]float64{}
	for _, it := range items {
		if it.ArmSkuName != strings.TrimPrefix(id, Prefix) || !slices.Contains(i.regions, it.ArmRegionName) || !it.IsPrimaryMeterRegion ||
			strings.Contains(it.ProductName, "Windows") {
			continue
		}
		var billing string
		price := it.RetailPrice
		switch {
		case it.Type == "Consumption" && strings.Contains(it.SkuName, "Low Priority"):
			continue
		case it.Type == "Consumption" && strings.Contains(it.SkuName, "Spot"):
			billing = model.BillingSpot
		case it.Type == "Consumption":
			billing = model.BillingOnDemand
		case it.Type == "Reservation" && it.ReservationTerm == "1 Year":
			// To the micro-dollar, as the API prices hours.
			billing, price = model.BillingReserved1Y, math.Round(price/hoursPerYear*1e6)/1e6
		default:
			continue
		}
		k := [2]string{it.ArmRegionName, billing}
		switch p, seen := usd[k]; {
		case it.CurrencyCode != "USD" || it.UnitOfMeasure != "1 Hour" || price < 0:
			return nil, fmt.Errorf("%s %s: %v %s per %s", k[0], k[1], it.RetailPrice, it.CurrencyCode, it.UnitOfMeasure)
		case seen && p != price:
			return nil, fmt.Errorf("%s %s: prices %v and %v", k[0], k[1], p, price)
		}
		usd[k] = price
	}
	if len(usd) == 0 {
		return nil, fmt.Errorf("no Linux on-demand, spot or 1-year reserved price in %s", strings.Join(i.regions, ", "))
	}
	rows := make([]model.NodeTypePrice, 0, len(usd))
	for k, p := range usd {
		rows = append(rows, model.NodeTypePrice{NodeTypeID: id, Region: k[0], Billing: k[1], USDPerHour: p})
	}
	slices.SortFunc(rows, func(a, b model.NodeTypePrice) int {
		return strings.Compare(a.Region+" "+a.Billing, b.Region+" "+b.Billing)
	})
	return rows, nil
}

// items reads the rows of the query rawURL, following each page's
// NextPageLink on the endpoint's origin only.
func (i *Importer) items(ctx context.Context, rawURL string) ([]item, error) {
	base, err := url.Parse(i.endpoint)
	if err != nil {
		return nil, err
	}
	var all []item
	for range maxPages {
		body, err := i.get(ctx, rawURL)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items        []item
			NextPageLink string
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("GET %s: %w", rawURL, err)
		}
		all = append(all, page.Items...)
		if page.NextPageLink == "" {
			return all, nil
		}
		next, err := url.Parse(page.NextPageLink)
		if err != nil || next.Scheme != base.Scheme || next.Hostname() != base.Hostname() || port(next) != port(base) {
			return nil, fmt.Errorf("GET %s: next page %q is not on %s", rawURL, page.NextPageLink, base.Host)
		}
		rawURL = page.NextPageLink
	}
	return nil, fmt.Errorf("GET %s: more than %d pages", rawURL, maxPages)
}

// port is u's port, its scheme's default if it names none.
func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return map[string]string{"http": "80", "https": "443"}[u.Scheme]
}

// get reads rawURL. It retries transport errors, 429 and 5xx up to retries
// times, after Retry-After (at most maxRetryAfter) when the response has one,
// else after a doubling backoff.
func (i *Importer) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		wait, retry := i.backoff<<attempt, true
		resp, err := i.client.Do(req)
		if err == nil {
			var body []byte
			body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
			resp.Body.Close()
			switch code := resp.StatusCode; {
			case err != nil:
			case code == http.StatusOK && len(body) > maxBody:
				err, retry = fmt.Errorf("body exceeds %d bytes", maxBody), false
			case code == http.StatusOK:
				return body, nil
			default:
				err = fmt.Errorf("HTTP %s", resp.Status)
				retry = code == http.StatusTooManyRequests || code >= 500
				if d, ok := retryAfter(resp.Header.Get("Retry-After")); ok {
					wait = d
				}
				if retry && wait > maxRetryAfter {
					err, retry = fmt.Errorf("%w (Retry-After %s exceeds %s)", err, wait, maxRetryAfter), false
				}
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retry || attempt == i.retries {
			return nil, fmt.Errorf("GET %s: %w", rawURL, err)
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// retryAfter reads a Retry-After of seconds or an HTTP date.
func retryAfter(v string) (time.Duration, bool) {
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
