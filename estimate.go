package catalog

import "math"

// Performance estimates (spec section 5) are a memory-bandwidth roofline, as AI
// Runway's estimator: one decode step for c concurrent sequences reads the
// active weights once and every sequence's KV cache.
//
//	step(c) = (active weights + c x KV at the context) /
//	    (BandwidthEfficiency x bandwidth x gpus x tpEfficiency(gpus)) seconds
//	per-sequence decode(c) = 1 / step(c) tokens/s
//	aggregate(c) = c / step(c) tokens/s
//	capacity = floor((EstimateMemoryUtilization x memory x gpus - weights) /
//	    KV at the context)
//
// DecodeTokensPerSec is decode(1). ConcurrentSequences is the largest c within
// capacity whose decode(c) is at least DecentDecodeTokensPerSec (0 when decode(1)
// is below it), AggregateTokensPerSec aggregate at that c. Active weights are
// the quantization's weights scaled by active over total parameters
// (mixture-of-experts); capacity counts all weights. Tokens/s are rounded to
// 0.1.
const (
	// BandwidthEfficiency is the share of peak memory bandwidth decode reaches.
	BandwidthEfficiency = 0.8
	// EstimateMemoryUtilization is the share of memory for weights and KV when
	// counting concurrent sequences.
	EstimateMemoryUtilization = 0.92
	// DecentDecodeTokensPerSec is the per-sequence decode rate deployment
	// guidance and concurrent sequences require, comfortably above reading
	// speed.
	DecentDecodeTokensPerSec = 20
)

// tpEfficiency is the share of the summed bandwidth tensor parallelism keeps.
func tpEfficiency(gpus int) float64 {
	switch {
	case gpus <= 1:
		return 1
	case gpus < 4:
		return 0.85
	}
	return 0.75
}

// Estimate is the performance of model m at quantization q on gpus GPUs of
// family f, at context tokens per sequence with a KV cache of kvDType.
func Estimate(m ModelTemplate, q Quantization, f Family, gpus int, context int, kvDType string) Performance {
	kv := kvGB(m, kvDType, context)
	active := q.WeightsGB
	if m.ParamsB > 0 && m.ParamsActiveB > 0 {
		active *= m.ParamsActiveB / m.ParamsB
	}
	rate := BandwidthEfficiency * f.MemoryBandwidthGBs * float64(gpus) * tpEfficiency(gpus)
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

// kvBytes is the KV cache per token at kvDType: fp8 halves the 16-bit default.
func kvBytes(m ModelTemplate, kvDType string) float64 {
	if kvDType == "fp8" {
		return m.kvBytesPerToken() / 2
	}
	return m.kvBytesPerToken()
}

func kvGB(m ModelTemplate, kvDType string, context int) float64 {
	return kvBytes(m, kvDType) * float64(context) / 1e9
}
