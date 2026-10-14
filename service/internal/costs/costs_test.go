// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package costs_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/costs"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

const (
	mi300x = "azure/Standard_ND96isr_MI300X_v5"
	h100   = "azure/Standard_ND96isr_H100_v5"
)

var asOf = time.Date(2026, 10, 9, 7, 54, 48, 0, time.UTC)

// committed generates the snapshot of the committed data/*.json.
func committed(t *testing.T) *catalog.CatalogSnapshot {
	t.Helper()
	in, _, err := bootstrap.Load("../../../data")
	if err != nil {
		t.Fatal(err)
	}
	snap, _, err := catalog.Generate(in.Families, in.NodeTypes, in.Models, in.Drafts, in.Backends)
	if err != nil {
		t.Fatal(err)
	}
	return &snap
}

func price(nodeType, region, billing string, usd float64) model.NodeTypePrice {
	return model.NodeTypePrice{Provenance: model.Provenance{Layer: model.LayerUpstream, Source: "azure-prices",
		SourceURL: "https://prices.azure.com/api/retail/prices", AsOf: asOf}, NodeTypeID: nodeType, Region: region, Billing: billing, USDPerHour: usd}
}

// line is an estimate without its run and price provenance.
func line(c model.CostEstimate) string {
	return fmt.Sprintf("%s %s %s %s %s %s: %d x = %v/h %v/month cheapest=%t", c.ModelSlug, c.Setup, c.FitKey, c.NodeTypeID, c.Region, c.Billing,
		c.Instances, c.USDPerHour, c.USDPerMonth, c.Cheapest)
}

// The spec's hand-computed example (eastus2, testdata/azure): one MI300X needs ceil(1/8) x 1 = 1 instance,
// the 2-node bf16 fit 2; 1-year reserved is 269107 USD / 8760 h = 30.719977/h.
func TestCostEstimateHandComputed(t *testing.T) {
	run := uuid.New()
	prices := []model.NodeTypePrice{
		price(mi300x, "eastus2", model.BillingOnDemand, 48),
		price(mi300x, "eastus2", model.BillingReserved1Y, 30.719977),
		price(mi300x, "eastus2", model.BillingSpot, 8.8704),
	}
	estimates := costs.Estimate(run, committed(t), prices)

	var optimal, twoNode []string
	for _, c := range estimates {
		if c.GeneratorRunID != run || c.PriceSource != "azure-prices" || c.PriceSourceURL != prices[0].SourceURL || !c.PriceAsOf.Equal(asOf) {
			t.Errorf("%s: run %s, price provenance %s %s %s", line(c), c.GeneratorRunID, c.PriceSource, c.PriceSourceURL, c.PriceAsOf)
		}
		if c.NodeTypeID != mi300x {
			t.Errorf("%s: estimated on a node type without a price", line(c))
		}
		switch {
		case c.ModelSlug == "llama-3.1-70b" && c.Setup == "optimal":
			optimal = append(optimal, line(c))
		case c.FitKey == "llama-3.1-70b/MI300X/-/bf16/auto/2-node":
			twoNode = append(twoNode, line(c))
		case c.ModelSlug == "llama-3.1-70b" && c.Setup == "minimum":
			t.Errorf("%s: the minimum setup (RTX-4090) has no priced node type", line(c))
		}
	}
	wantOptimal := []string{
		"llama-3.1-70b optimal  " + mi300x + " eastus2 on_demand: 1 x = 48/h 35040/month cheapest=true",
		"llama-3.1-70b optimal  " + mi300x + " eastus2 reserved_1y: 1 x = 30.719977/h 22425.58321/month cheapest=true",
		"llama-3.1-70b optimal  " + mi300x + " eastus2 spot: 1 x = 8.8704/h 6475.392/month cheapest=true",
	}
	if !slices.Equal(optimal, wantOptimal) {
		t.Errorf("llama-3.1-70b optimal =\n%q\nwant\n%q", optimal, wantOptimal)
	}
	if want := "llama-3.1-70b fit llama-3.1-70b/MI300X/-/bf16/auto/2-node " + mi300x + " eastus2 on_demand: 2 x = 96/h 70080/month cheapest=false"; len(twoNode) != 3 || twoNode[0] != want {
		t.Errorf("2-node fit = %q, want first %q", twoNode, want)
	}
}

// Exactly one estimate per model, guidance setup and billing type is cheapest; fits are never marked.
func TestCheapestGuidanceSetup(t *testing.T) {
	snap := committed(t)
	estimates := costs.Estimate(uuid.New(), snap, []model.NodeTypePrice{
		price(h100, "eastus", model.BillingOnDemand, 98.32),
		price(mi300x, "eastus2", model.BillingOnDemand, 48),
		price(mi300x, "eastus2", model.BillingSpot, 8.8704),
		price(mi300x, "westus3", model.BillingOnDemand, 47.5),
	})
	cheapest := map[string][]string{}
	for _, c := range estimates {
		if c.Cheapest {
			k := c.ModelSlug + " " + c.Setup + " " + c.Billing
			cheapest[k] = append(cheapest[k], c.NodeTypeID+" "+c.Region)
		}
	}
	for k, v := range cheapest {
		if len(v) != 1 {
			t.Errorf("%s: %d estimates marked cheapest: %v", k, len(v), v)
		}
	}
	want := map[string][]string{
		"llama-3.1-70b optimal on_demand": {mi300x + " westus3"},
		"llama-3.1-70b optimal spot":      {mi300x + " eastus2"},
	}
	for k, v := range want {
		if !reflect.DeepEqual(cheapest[k], v) {
			t.Errorf("%s cheapest = %v, want %v", k, cheapest[k], v)
		}
	}
	for _, c := range estimates {
		if c.Setup == "fit" && c.Cheapest {
			t.Errorf("%s: a fit marked cheapest", line(c))
		}
	}
}
