package catalog

import (
	"math"
	"testing"
)

// Hand-computed from the constants in estimate.go.
//
// llama-3.1-8b bf16 on one H100 (80GB, 3350GB/s) at 8192 tokens:
// KV = 131072B x 8192 = 1.073741824GB;
// decode = 0.8 x 3350 x 1.0 x 1 / (16.1 + 1.0737) = 2680 / 17.1737 = 156.05 tokens/s;
// concurrent = floor((0.92 x 80 x 1 - 16.1) / 1.0737) = floor(53.55) = 53;
// aggregate = 156.05 x 53 x 0.6 = 4962.46 tokens/s.
//
// mixtral-8x7b bf16 (93.4GB, 12.9B of 47B parameters active) on two H100 at
// 32768 tokens: KV = 131072B x 32768 = 4.294967296GB; active weights =
// 93.4 x 12.9 / 47 = 25.635GB;
// decode = 0.8 x 3350 x 0.85 x 2 / (25.635 + 4.295) = 4556 / 29.930 = 152.22 tokens/s;
// concurrent = floor((0.92 x 80 x 2 - 93.4) / 4.295) = floor(12.53) = 12;
// aggregate = 152.22 x 12 x 0.6 = 1095.99 tokens/s.
func TestEstimateHandComputed(t *testing.T) {
	h100 := Family{Name: "H100", MemoryGB: 80, MemoryBandwidthGBs: 3350}
	mixtral := ModelTemplate{Slug: "mixtral-8x7b", ParamsB: 47, ParamsActiveB: 12.9,
		Quantizations: []Quantization{{Name: "bf16", Kind: "float", WeightsGB: 93.4}},
		Architecture:  Architecture{Layers: 32, AttentionHeads: 32, KVHeads: 8, HeadDim: 128}}
	cases := []struct {
		m                 ModelTemplate
		gpus, context     int
		decode, aggregate float64
		concurrent        int
	}{
		{llama8b(), 1, 8192, 156.05, 4962.46, 53},
		{mixtral, 2, 32768, 152.22, 1095.99, 12},
	}
	for _, c := range cases {
		p := Estimate(c.m, c.m.Quantizations[0], h100, c.gpus, c.context, "auto")
		if math.Abs(p.DecodeTokensPerSec-c.decode) > c.decode*0.005 || math.Abs(p.AggregateTokensPerSec-c.aggregate) > c.aggregate*0.005 ||
			p.ConcurrentSequences != c.concurrent || p.Context != c.context || p.Source != "estimate" {
			t.Errorf("%s: %+v; want decode %v, %d sequences, aggregate %v at %d tokens, source estimate",
				c.m.Slug, p, c.decode, c.concurrent, c.aggregate, c.context)
		}
	}
}
