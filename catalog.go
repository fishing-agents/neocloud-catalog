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
const SchemaVersion = "1.0.0"

type Quantization struct {
	Name      string  `json:"name"`
	WeightsGB float64 `json:"weights_gb"`
}

// ModelTemplate describes a model an operator can launch: its weights per
// quantization, its KV-cache bytes per token (used to size VRAM for any context),
// and the context limits the vendor curates.
type ModelTemplate struct {
	Slug            string         `json:"slug"`
	Name            string         `json:"name"`
	Family          string         `json:"family"`
	ParamsB         float64        `json:"params_b"`
	DefaultContext  int            `json:"default_context"`
	ContextMax      int            `json:"context_max"`
	Quantizations   []Quantization `json:"quantizations"`
	KVBytesPerToken float64        `json:"kv_bytes_per_token"`
	Tags            []string       `json:"tags,omitempty"`
}

// ModelFit is a curated row of "which device family (and profile) runs which
// model", stating the largest context that fits, the VRAM that requires, and the
// expected throughput. Profile is nil for the full-GPU case. This is the
// requirements-to-vendor-GPU-class mapping: a model's needs against a GPU's
// capability, with an expected performance metric for sizing/placement.
type ModelFit struct {
	ModelSlug      string  `json:"model_slug"`
	DeviceFamily   string  `json:"device_family"`
	Profile        *string `json:"profile,omitempty"`
	MaxContext     int     `json:"max_context"`
	VRAMRequiredGB float64 `json:"vram_required_gb"`
	TokensPerSec   float64 `json:"tokens_per_sec"`
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
	var weights float64
	found := false
	for _, q := range t.Quantizations {
		if q.Name == quant {
			weights = q.WeightsGB
			found = true
			break
		}
	}
	if !found {
		return 0, false
	}
	return weights + t.KVBytesPerToken*float64(context)/1e9, true
}

// Fit reports whether a model runs on a fit row at the given context: within the
// curated ceiling and within the row's VRAM budget for the chosen quantization.
func Fit(t ModelTemplate, f ModelFit, quant string, context int) bool {
	if context > f.MaxContext {
		return false
	}
	vram, ok := VRAMRequired(t, quant, context)
	if !ok {
		return false
	}
	return vram <= f.VRAMRequiredGB
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
