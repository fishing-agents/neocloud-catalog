package catalog

import (
	"math"
	"testing"
)

// Hand-computed from the constants in estimate.go: step(c) = (active weights +
// c x KV) / (0.8 x bandwidth x gpus x tp efficiency), per-sequence decode =
// 1 / step(c), aggregate = c / step(c), c the most sequences that fit in 0.92 of
// memory while each decodes at least 20 tokens/s.
//
// llama-3.1-8b bf16 on one H100 (80GB, 3350GB/s) at 8192 tokens: KV =
// 131072B x 8192 = 1.0737GB; rate = 0.8 x 3350 = 2680GB/s; decode =
// 2680 / (16.1 + 1.0737) = 156.05; capacity = floor((73.6 - 16.1) / 1.0737) = 53;
// 20 tokens/s holds up to (2680 / 20 - 16.1) / 1.0737 = 109.8, so c = 53;
// aggregate = 53 x 2680 / (16.1 + 53 x 1.0737) = 1945.53.
//
// mixtral-8x7b bf16 (93.4GB, 12.9B of 47B parameters active) on two H100 at
// 32768 tokens: KV = 4.2950GB; active = 93.4 x 12.9 / 47 = 25.635GB; rate =
// 0.8 x 3350 x 2 x 0.85 = 4556GB/s; decode = 4556 / 29.930 = 152.22; capacity =
// floor((147.2 - 93.4) / 4.295) = 12 (20 tokens/s up to 47.1); aggregate =
// 12 x 4556 / (25.635 + 12 x 4.295) = 708.42.
//
// llama-3.1-8b fp8 (9.1GB) on one L4 (24GB, 300.1GB/s) at 8192 tokens: rate =
// 240.08GB/s; decode = 240.08 / 10.174 = 23.60; capacity = floor((22.08 - 9.1) /
// 1.0737) = 12 but 20 tokens/s holds only up to (12.004 - 9.1) / 1.0737 = 2.70,
// so c = 2; aggregate = 2 x 240.08 / (9.1 + 2.1475) = 42.69.
//
// llama-3.1-8b bf16 on one L4: decode = 240.08 / 17.174 = 13.98, below 20 even
// alone: c = 0, aggregate 0.
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
