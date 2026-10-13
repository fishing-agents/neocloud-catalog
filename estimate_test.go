// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"math"
	"testing"
)

// Hand-computed from the constants in estimate.go, rate = 0.8 x bandwidth x gpus x tp:
//
//	8b bf16, H100, 8192: KV 1.0737GB, decode 2680/17.174 = 156.05, c = 53 (capacity).
//	mixtral bf16, 2 H100, 32768: active 25.635GB, rate 4556, c = 12 (capacity).
//	8b fp8, L4: capacity 12 but 20 tokens/s holds to 2.70 sequences, so c = 2.
//	8b bf16, L4: decode 13.98 < 20 even alone, so c = 0.
func TestEstimateHandComputed(t *testing.T) {
	h100 := Family{Name: "H100", MemoryGB: 80, MemoryBandwidthGBs: 3350}
	l4 := Family{Name: "L4", MemoryGB: 24, MemoryBandwidthGBs: 300.1}
	mixtral := ModelTemplate{Slug: "mixtral-8x7b", ParamsB: 47, ParamsActiveB: 12.9,
		Quantizations: []Quantization{{Name: "bf16", Kind: "float", WeightsGB: 93.4}},
		Architecture:  Architecture{Layers: 32, AttentionHeads: 32, KVHeads: 8, HeadDim: 128}}
	llama := llama8b()
	cases := []struct {
		name              string
		m                 ModelTemplate
		q                 Quantization
		f                 Family
		gpus, context     int
		decode, aggregate float64
		concurrent        int
	}{
		{"llama-3.1-8b bf16 on H100", llama, llama.Quantizations[0], h100, 1, 8192, 156.05, 1945.53, 53},
		{"mixtral-8x7b bf16 on 2 H100", mixtral, mixtral.Quantizations[0], h100, 2, 32768, 152.22, 708.42, 12},
		{"llama-3.1-8b fp8 on L4", llama, llama.Quantizations[1], l4, 1, 8192, 23.60, 42.69, 2},
		{"llama-3.1-8b bf16 on L4", llama, llama.Quantizations[0], l4, 1, 8192, 13.98, 0, 0},
	}
	for _, c := range cases {
		p := Estimate(c.m, c.q, c.f, c.gpus, c.context, "auto")
		if math.Abs(p.DecodeTokensPerSec-c.decode) > c.decode*0.005 || math.Abs(p.AggregateTokensPerSec-c.aggregate) > c.aggregate*0.005 ||
			p.ConcurrentSequences != c.concurrent || p.Context != c.context || p.Source != "estimate" {
			t.Errorf("%s: %+v; want decode %v, %d sequences, aggregate %v at %d tokens, source estimate",
				c.name, p, c.decode, c.concurrent, c.aggregate, c.context)
		}
	}
}
