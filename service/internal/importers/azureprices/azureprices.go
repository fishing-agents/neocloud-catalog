// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package azureprices imports Azure Retail Prices (Linux VM on-demand, spot and
// 1-year reserved, USD/hour) for Azure node types into the upstream layer.
// A node type with no price in any configured region loses its prices.
package azureprices

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/fetch"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const (
	Endpoint = "https://prices.azure.com/api/retail/prices"
	// Prefix plus the ARM SKU name is an Azure node type id.
	Prefix = "azure/"
	// A page holds at most 1000 rows of about 600 bytes.
	maxBody  = 4 << 20
	maxPages = 20
	// A longer Retry-After fails the node type rather than spend jobs.job_timeout.
	maxRetryAfter = 5 * time.Minute
	// The API gives a 1-year reservation's price for the whole term although
	// unitOfMeasure says "1 Hour".
	hoursPerYear = 365 * 24
)

// sku keeps the OData filter well formed.
var sku = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// Importer reads the Retail Prices API at endpoint in regions.
type Importer struct {
	endpoint string
	regions  []string
	getter   fetch.Getter
}

// New returns the importer of the API at endpoint in regions.
func New(endpoint string, regions []string) *Importer {
	return &Importer{endpoint: endpoint, regions: regions,
		getter: fetch.Getter{Client: &http.Client{Timeout: time.Minute}, Backoff: time.Second, Retries: 3, MaxBody: maxBody, MaxRetryAfter: maxRetryAfter}}
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
		p.Rows = append(p.Rows, rows...)
		if len(rows) == 0 {
			sum.Errors = append(sum.Errors, id+": no Linux on-demand, spot or 1-year reserved price in "+strings.Join(i.regions, ", "))
			sum.Sources.Failed++
			continue
		}
		sum.Sources.Read++
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
	if sum.Sources.Read == 0 && len(ids) > 0 {
		return sum, fmt.Errorf("no node type's prices could be read (Azure node types %q): %s", ids, strings.Join(sum.Errors, "; "))
	}
	sum.Changes, _, err = s.Patch(ctx, run, store.Patch{Prices: &p})
	return sum, err
}

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

type item struct {
	ArmSkuName, ArmRegionName, SkuName, ProductName    string
	Type, ReservationTerm, UnitOfMeasure, CurrencyCode string
	RetailPrice                                        float64
	IsPrimaryMeterRegion                               bool
}

// Low Priority is not priced. Two prices of one region and billing, or a
// price not in USD per hour, fail the node type.
func (i *Importer) prices(id string, items []item) ([]model.NodeTypePrice, error) {
	usd := map[[2]string]float64{}
	for _, it := range items {
		if it.ArmSkuName != strings.TrimPrefix(id, Prefix) || !slices.Contains(i.regions, it.ArmRegionName) || !it.IsPrimaryMeterRegion ||
			strings.Contains(it.ProductName, "Windows") || it.RetailPrice == 0 {
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
	rows := make([]model.NodeTypePrice, 0, len(usd))
	for k, p := range usd {
		rows = append(rows, model.NodeTypePrice{NodeTypeID: id, Region: k[0], Billing: k[1], USDPerHour: p})
	}
	slices.SortFunc(rows, func(a, b model.NodeTypePrice) int {
		return strings.Compare(a.Region+" "+a.Billing, b.Region+" "+b.Billing)
	})
	return rows, nil
}

// items follows NextPageLink on the endpoint's origin only.
func (i *Importer) items(ctx context.Context, rawURL string) ([]item, error) {
	base, err := url.Parse(i.endpoint)
	if err != nil {
		return nil, err
	}
	var all []item
	for range maxPages {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		body, _, err := i.getter.Get(ctx, req)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", rawURL, err)
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

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return map[string]string{"http": "80", "https": "443"}[u.Scheme]
}
