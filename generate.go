package catalog

import "sort"

// FamilyCapability is the curated per-family GPU memory used to derive fits.
// This is the outsourced capability table - neocloud no longer derives fits from
// its own device catalog at seed time; the generator does it here.
type FamilyCapability struct {
	Name     string  `json:"name"`
	MemoryGB float64 `json:"memory_gb"`
}

// Generate derives the fit matrix from curated templates and family capabilities.
// A family fits a model when its memory exceeds the lead (fp16) weights; max
// context is the largest context the remaining memory holds at KV bytes/token,
// capped at the model's ceiling; throughput is a linear family heuristic. Pure and
// deterministic, so the emitted snapshot's digest is stable for identical input.
func Generate(templates []ModelTemplate, families []FamilyCapability) CatalogSnapshot {
	snap := CatalogSnapshot{
		SchemaVersion: SchemaVersion,
		Models:        templates,
		Fits:          []ModelFit{},
	}
	for _, fam := range families {
		for _, t := range templates {
			if len(t.Quantizations) == 0 {
				continue
			}
			weights := t.Quantizations[0].WeightsGB
			if fam.MemoryGB <= weights {
				continue
			}
			fitCtx := int((fam.MemoryGB - weights) * 1e9 / t.KVBytesPerToken)
			if fitCtx > t.ContextMax {
				fitCtx = t.ContextMax
			}
			if fitCtx <= 0 {
				continue
			}
			snap.Fits = append(snap.Fits, ModelFit{
				ModelSlug:      t.Slug,
				DeviceFamily:   fam.Name,
				MaxContext:     fitCtx,
				VRAMRequiredGB: weights,
				TokensPerSec:   400 + fam.MemoryGB*10,
			})
		}
	}
	sort.Slice(snap.Fits, func(i, j int) bool {
		if snap.Fits[i].ModelSlug != snap.Fits[j].ModelSlug {
			return snap.Fits[i].ModelSlug < snap.Fits[j].ModelSlug
		}
		return snap.Fits[i].DeviceFamily < snap.Fits[j].DeviceFamily
	})
	return snap
}
