package catalog

import (
	"math"
	"sort"
)

// GPUMemoryUtilization is the share of each GPU's memory a model may use for
// weights and KV cache; vLLM's default, leaving the rest for activations.
const GPUMemoryUtilization = 0.9

// tpSizes are the tensor-parallel GPU counts considered, smallest first.
var tpSizes = []int{1, 2, 4, 8}

// FamilyCapability is the curated per-family GPU memory used to derive fits.
type FamilyCapability struct {
	Name     string  `json:"name"`
	MemoryGB float64 `json:"memory_gb"`
}

// Generate derives the fit matrix. For each model, quantization, and family it
// emits the smallest legal GPU count whose memory budget holds the weights plus
// the model's default context; max context is what the remaining budget holds,
// capped at the model's ceiling. Pure and deterministic, so the snapshot digest
// is stable for identical input.
func Generate(templates []ModelTemplate, families []FamilyCapability) CatalogSnapshot {
	snap := CatalogSnapshot{SchemaVersion: SchemaVersion, Models: make([]ModelTemplate, len(templates)), Fits: []ModelFit{}}
	for i, t := range templates {
		t.KVBytesPerToken = t.Architecture.KVBytesPerToken()
		snap.Models[i] = t
		if t.KVBytesPerToken <= 0 {
			continue
		}
		for _, q := range t.Quantizations {
			for _, fam := range families {
				if f, ok := fit(t, q, fam); ok {
					snap.Fits = append(snap.Fits, f)
				}
			}
		}
	}
	sort.Slice(snap.Fits, func(i, j int) bool {
		a, b := snap.Fits[i], snap.Fits[j]
		if a.ModelSlug != b.ModelSlug {
			return a.ModelSlug < b.ModelSlug
		}
		if a.DeviceFamily != b.DeviceFamily {
			return a.DeviceFamily < b.DeviceFamily
		}
		return a.Quantization < b.Quantization
	})
	return snap
}

func fit(t ModelTemplate, q Quantization, fam FamilyCapability) (ModelFit, bool) {
	for _, gpus := range tpDegrees(t.Architecture.AttentionHeads) {
		budget := fam.MemoryGB * GPUMemoryUtilization * float64(gpus)
		ctx := min(int((budget-q.WeightsGB)*1e9/t.KVBytesPerToken), t.ContextMax)
		if ctx < min(t.DefaultContext, t.ContextMax) {
			continue
		}
		return ModelFit{
			ModelSlug:      t.Slug,
			DeviceFamily:   fam.Name,
			Quantization:   q.Name,
			GPUCount:       gpus,
			MaxContext:     ctx,
			VRAMRequiredGB: math.Ceil((q.WeightsGB+t.KVBytesPerToken*float64(ctx)/1e9)*100) / 100,
		}, true
	}
	return ModelFit{}, false
}

// tpDegrees are the GPU counts tensor parallelism allows: those dividing the
// attention head count.
func tpDegrees(heads int) []int {
	var out []int
	for _, n := range tpSizes {
		if heads > 0 && heads%n == 0 {
			out = append(out, n)
		}
	}
	return out
}
