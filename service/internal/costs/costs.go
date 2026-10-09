// Package costs estimates, internally, what the hardware of a snapshot's fits
// and guidance setups costs to run on a cloud: per node type of the setup's
// family that has a price, per region and billing type, the instances needed
// and their price per hour and per month.
package costs

import (
	"math"

	"github.com/google/uuid"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

// HoursPerMonth is the month a price per hour is multiplied by.
const HoursPerMonth = 730

// Estimate returns the cost estimates of generator run run: for every fit and
// guidance setup of snap and every node type of its family with a price in
// prices (one per node type, region and billing type), instances =
// ceil(GPUs per node / node type GPUs) x nodes, at instances x the price per
// hour. Per model, guidance setup and billing type the cheapest estimate,
// first in node type and price order on a tie, is marked.
func Estimate(run uuid.UUID, snap *catalog.CatalogSnapshot, prices []model.NodeTypePrice) []model.CostEstimate {
	priced := map[string][]model.NodeTypePrice{}
	for _, p := range prices {
		priced[p.NodeTypeID] = append(priced[p.NodeTypeID], p)
	}
	var out []model.CostEstimate
	add := func(slug, setup, fitKey, family string, gpus, nodes int) {
		perNode := (gpus + nodes - 1) / nodes
		for _, nt := range snap.NodeTypes {
			if nt.Family != family {
				continue
			}
			instances := (perNode + nt.GPUs - 1) / nt.GPUs * nodes
			for _, p := range priced[nt.ID] {
				hour := micros(p.USDPerHour) * int64(instances)
				out = append(out, model.CostEstimate{Derivation: model.Derivation{GeneratorRunID: run}, ModelSlug: slug, Setup: setup,
					FitKey: fitKey, NodeTypeID: nt.ID, Region: p.Region, Billing: p.Billing, Instances: instances, USDPerHour: dollars(hour),
					USDPerMonth: dollars(hour * HoursPerMonth), PriceSource: p.Source, PriceSourceURL: p.SourceURL, PriceAsOf: p.AsOf})
			}
		}
	}
	for _, f := range snap.Fits {
		add(f.ModelSlug, "fit", catalog.FitKey(f), f.DeviceFamily, f.GPUCount, f.Nodes)
	}
	for _, m := range snap.Models {
		for _, g := range []struct {
			setup string
			r     *catalog.Recommendation
		}{{"minimum", m.Deployment.Minimum}, {"optimal", m.Deployment.Optimal}} {
			if g.r == nil {
				continue
			}
			start := len(out)
			add(m.Slug, g.setup, "", g.r.Family, g.r.GPUs, g.r.Nodes)
			cheapest := map[string]*model.CostEstimate{}
			for i := range out[start:] {
				c := &out[start+i]
				if low := cheapest[c.Billing]; low == nil || c.USDPerHour < low.USDPerHour {
					cheapest[c.Billing] = c
				}
			}
			for _, c := range cheapest {
				c.Cheapest = true
			}
		}
	}
	return out
}

// micros is usd in micro-dollars: estimates are exact to the micro-dollar,
// the precision Azure gives prices per hour in, rather than carry binary
// floating point error into sums.
func micros(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

func dollars(m int64) float64 { return float64(m) / 1e6 }
