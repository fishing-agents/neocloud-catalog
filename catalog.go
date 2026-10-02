// Package catalog is the model-template catalog core, split out of neocloud as a
// public, CNCF-donation-ready module. It is pure and dependency-free (stdlib
// only): no GORM, no tenancy, no HTTP. The serialized CatalogSnapshot is the
// embeddable artifact clients pull and pin by digest. Curation and fit derivation
// live here (the outsourced generation); neocloud ingests the committed snapshot.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// SchemaVersion is the versioned wire format. Bump it on any breaking change to
// the snapshot shape so embedders can gate on it.
const SchemaVersion = "2.0.0"

type Quantization struct {
	Name      string  `json:"name"`
	WeightsGB float64 `json:"weights_gb"`
}

// Architecture is the subset of a model's config that sizes its KV cache and
// constrains tensor parallelism (attention heads must divide the GPU count).
type Architecture struct {
	Layers         int `json:"layers"`
	AttentionHeads int `json:"attention_heads"`
	KVHeads        int `json:"kv_heads"`
	HeadDim        int `json:"head_dim"`
}

// KVBytesPerToken is K and V for every layer and KV head at a 2-byte (16-bit)
// cache dtype, vLLM's default for 16-bit models.
func (a Architecture) KVBytesPerToken() float64 {
	return float64(2 * a.Layers * a.KVHeads * a.HeadDim * 2)
}

// ModelTemplate describes a model an operator can launch: its weights per
// quantization, its architecture, and the context limits the vendor curates.
// KVBytesPerToken is derived from Architecture by Generate.
type ModelTemplate struct {
	Slug            string         `json:"slug"`
	Name            string         `json:"name"`
	Family          string         `json:"family"`
	ParamsB         float64        `json:"params_b"`
	DefaultContext  int            `json:"default_context"`
	ContextMax      int            `json:"context_max"`
	Quantizations   []Quantization `json:"quantizations"`
	Architecture    Architecture   `json:"architecture"`
	KVBytesPerToken float64        `json:"kv_bytes_per_token"`
	Tags            []string       `json:"tags,omitempty"`
}

// kvBytesPerToken prefers the stored value (snapshots assembled elsewhere may
// omit architecture) and derives it otherwise.
func (t ModelTemplate) kvBytesPerToken() float64 {
	if t.KVBytesPerToken > 0 {
		return t.KVBytesPerToken
	}
	return t.Architecture.KVBytesPerToken()
}

// ModelFit states that a model at a quantization runs on GPUCount GPUs of a
// device family (tensor parallel), with the largest context that fits and the
// VRAM that context needs across those GPUs, rounded up to 0.01 GB. Profile is
// nil for whole GPUs.
type ModelFit struct {
	ModelSlug      string  `json:"model_slug"`
	DeviceFamily   string  `json:"device_family"`
	Profile        *string `json:"profile,omitempty"`
	Quantization   string  `json:"quantization"`
	GPUCount       int     `json:"gpu_count"`
	MaxContext     int     `json:"max_context"`
	VRAMRequiredGB float64 `json:"vram_required_gb"`
}

// CatalogSnapshot is the immutable, versioned payload served to clients. Version
// changes when the catalog changes; Digest is the content hash clients pin and
// the ETag used for conditional gets.
type CatalogSnapshot struct {
	SchemaVersion string          `json:"schema_version"`
	Version       string          `json:"version"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Models        []ModelTemplate `json:"models"`
	Fits          []ModelFit      `json:"fits"`
}

// VRAMRequired computes the VRAM a model needs at a given context for the named
// quantization: weights plus the KV cache that grows with context. Returns false
// if the quantization is unknown.
func VRAMRequired(t ModelTemplate, quant string, context int) (float64, bool) {
	for _, q := range t.Quantizations {
		if q.Name == quant {
			return q.WeightsGB + t.kvBytesPerToken()*float64(context)/1e9, true
		}
	}
	return 0, false
}

// Fit reports whether a model at a quantization runs on a fit row at the given
// context: same quantization, within the row's context, within its VRAM.
func Fit(t ModelTemplate, f ModelFit, quant string, context int) bool {
	if quant != f.Quantization || context > f.MaxContext {
		return false
	}
	vram, ok := VRAMRequired(t, quant, context)
	return ok && vram <= f.VRAMRequiredGB
}

// Digest returns the sha256 of the snapshot's content: schema version, version,
// and rows. GeneratedAt is excluded so identical content yields an identical
// digest regardless of when it was assembled.
func (s CatalogSnapshot) Digest() string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode(struct {
		SchemaVersion string          `json:"schema_version"`
		Version       string          `json:"version"`
		Models        []ModelTemplate `json:"models"`
		Fits          []ModelFit      `json:"fits"`
	}{s.SchemaVersion, s.Version, s.Models, s.Fits})
	return hex.EncodeToString(h.Sum(nil))
}
