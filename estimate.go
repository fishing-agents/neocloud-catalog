package catalog

import "math"

// Performance estimates (spec section 5) are a memory-bandwidth roofline, as AI
// Runway's estimator: decoding one token reads the active weights and the
// sequence's KV cache once.
//
//	decode tokens/s per sequence = BandwidthEfficiency x bandwidth x
//	    tpEfficiency(gpus) x gpus / (active weights + KV at the context)
//	concurrent sequences = floor((EstimateMemoryUtilization x memory x gpus -
//	    weights) / KV at the context)
//	aggregate tokens/s = per-sequence x concurrent x BatchingEfficiency
//
// Active weights are the quantization's weights scaled by active over total
// parameters (mixture-of-experts). Tokens/s are rounded to 0.1.
const (
	// BandwidthEfficiency is the share of peak memory bandwidth decode reaches.
	BandwidthEfficiency = 0.8
	// EstimateMemoryUtilization is the share of memory for weights and KV when
	// counting concurrent sequences.
	EstimateMemoryUtilization = 0.92
	// BatchingEfficiency caps the aggregate of concurrent sequences.
	BatchingEfficiency = 0.6
	// DecentDecodeTokensPerSec is the per-sequence decode rate deployment
	// guidance requires, comfortably above reading speed.
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
	decode := BandwidthEfficiency * f.MemoryBandwidthGBs * tpEfficiency(gpus) * float64(gpus) / (active + kv)
	concurrent := 0
	if kv > 0 {
		concurrent = max(0, int((EstimateMemoryUtilization*f.MemoryGB*float64(gpus)-q.WeightsGB)/kv))
	}
	return Performance{
		Source:                "estimate",
		DecodeTokensPerSec:    round1(decode),
		ConcurrentSequences:   concurrent,
		AggregateTokensPerSec: round1(decode * float64(concurrent) * BatchingEfficiency),
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
