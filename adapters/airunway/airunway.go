// Package airunway exports the catalog to AI Runway: recipes for its vLLM
// recipes resolver (served from VLLM_RECIPES_BASE_URL) and, for vLLM and
// SGLang alike, ModelDeployment manifests with the matching bodies for
// POST /api/deployments.
package airunway

import (
	"bytes"
	"encoding/json"
	"fmt"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

// Formats.
const (
	FormatRecipes     = "airunway-recipes"
	FormatDeployments = "airunway-deployments"
)

// Skip reasons, per format.
const (
	SkipNotVLLM    = "recipes resolver is vLLM only"
	SkipRecipeNoHF = "recipes need a Hugging Face org/model repo"
	SkipRepoTaken  = "one recipe per Hugging Face repo"
	SkipBackend    = "backend AI Runway cannot run"
	SkipNoHF       = "ModelDeployment source is huggingface or custom only"
	SkipNoFit      = "no whole-GPU fit to size the GPU count"
)

type adapter struct{}

func init() { adapters.Register(adapter{}) }

func (adapter) Name() string { return "airunway" }

func (adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{
		Backends: []string{"vllm", "sglang"},
		Sources:  []string{catalog.SourceHuggingFace},
	}
}

func (adapter) Formats() []string { return []string{FormatRecipes, FormatDeployments} }

func (adapter) SkipReasons(format string) []string {
	switch format {
	case FormatRecipes:
		return []string{SkipNotVLLM, SkipRecipeNoHF, SkipRepoTaken, SkipNoFit}
	case FormatDeployments:
		return []string{SkipBackend, SkipNoHF, SkipNoFit}
	}
	return nil
}

func (adapter) Export(snap catalog.CatalogSnapshot, format string, opts adapters.Options) (adapters.Export, error) {
	switch format {
	case FormatRecipes:
		return exportRecipes(snap, opts)
	case FormatDeployments:
		return exportDeployments(snap, opts)
	}
	return adapters.Export{}, fmt.Errorf("airunway: unknown format %q", format)
}

// target is a preset with what AI Runway needs to run it.
type target struct {
	adapters.Ref
	source  catalog.ModelSource
	release catalog.BackendVersion
	fit     catalog.ModelFit
}

// resolve finds a preset's Hugging Face source, pinned release and reference
// fit, or returns the reason it cannot be expressed (noHF without a source).
func resolve(snap catalog.CatalogSnapshot, r adapters.Ref, opts adapters.Options, noHF string) (target, string, error) {
	src, ok := r.Source(catalog.SourceHuggingFace)
	if !ok {
		return target{}, noHF, nil
	}
	rel, ok := adapters.BackendVersion(snap, r.Preset.Backend, r.Preset.BackendVersion)
	if !ok {
		return target{}, "", fmt.Errorf("%s/%s: backend %s %s is not in the snapshot", r.Model.Slug, r.Preset.Name, r.Preset.Backend, r.Preset.BackendVersion)
	}
	fit, ok := adapters.ReferenceFit(snap, r.Model.Slug, r.Preset.Quantization, opts)
	if !ok {
		return target{}, SkipNoFit, nil
	}
	return target{Ref: r, source: src, release: rel, fit: fit}, "", nil
}

func skip(e *adapters.Export, r adapters.Ref, reason string) {
	e.Skipped = append(e.Skipped, adapters.Skip{PresetKey: r.Key(), Reason: reason})
}

// encode writes indented JSON, which AI Runway reads as JSON and kubectl as
// YAML.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(v)
	return b.Bytes(), err
}
