// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package catalog

import "math"

// Estimates are a memory-bandwidth roofline: a decode step for c sequences reads
// the active weights (scaled by active/total params) once plus every KV cache.
//
//	step(c) = (active + c x KV) / (BandwidthEfficiency x bandwidth x gpus
//	    x tpEfficiency(gpus per stage) x CrossNodeEfficiency^(stages-1))
//	decode(c) = 1/step(c); aggregate(c) = c/step(c)
//	capacity = floor((EstimateMemoryUtilization x memory x gpus - weights) / KV)
//
// ConcurrentSequences is the largest c within capacity with decode(c) >=
// DecentDecodeTokensPerSec, or 0.
const (
	// BandwidthEfficiency is the share of peak memory bandwidth decode reaches.
	BandwidthEfficiency = 0.8
	// EstimateMemoryUtilization is the memory share for weights and KV when counting sequences.
	EstimateMemoryUtilization = 0.92
	// DecentDecodeTokensPerSec is the per-sequence rate guidance requires, above reading speed.
	DecentDecodeTokensPerSec = 20
	// CrossNodeEfficiency is the throughput kept per pipeline stage boundary.
	CrossNodeEfficiency = 0.85
)

// tpEfficiency is the share of summed bandwidth tensor parallelism keeps.
func tpEfficiency(gpus int) float64 {
	switch {
	case gpus <= 1:
		return 1
	case gpus < 4:
		return 0.85
	}
	return 0.75
}

// Estimate is m's performance at q on gpus GPUs of f, at context tokens per sequence.
func Estimate(m ModelTemplate, q Quantization, f Family, gpus int, context int, kvDType string) Performance {
	return estimate(m, q, f, gpus, 1, context, kvDType)
}

// estimate is Estimate across stages pipeline stages of gpus/stages GPUs each.
func estimate(m ModelTemplate, q Quantization, f Family, gpus, stages int, context int, kvDType string) Performance {
	kv := kvGB(m, kvDType, context)
	active := q.WeightsGB
	if m.ParamsB > 0 && m.ParamsActiveB > 0 {
		active *= m.ParamsActiveB / m.ParamsB
	}
	rate := BandwidthEfficiency * f.MemoryBandwidthGBs * float64(gpus) * tpEfficiency(gpus/stages) * math.Pow(CrossNodeEfficiency, float64(stages-1))
	decode := func(c int) float64 { return rate / (active + float64(c)*kv) }
	c := 0
	if kv > 0 {
		capacity := int((EstimateMemoryUtilization*f.MemoryGB*float64(gpus) - q.WeightsGB) / kv)
		for c < capacity && decode(c+1) >= DecentDecodeTokensPerSec {
			c++
		}
	}
	return Performance{
		Source:                "estimate",
		DecodeTokensPerSec:    round1(decode(1)),
		ConcurrentSequences:   c,
		AggregateTokensPerSec: round1(float64(c) * decode(c)),
		Context:               context,
	}
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

// kvBytes is the KV cache per token: fp8 halves the 16-bit default.
func kvBytes(m ModelTemplate, kvDType string) float64 {
	if kvDType == "fp8" {
		return m.kvBytesPerToken() / 2
	}
	return m.kvBytesPerToken()
}

func kvGB(m ModelTemplate, kvDType string, context int) float64 {
	return kvBytes(m, kvDType) * float64(context) / 1e9
}
