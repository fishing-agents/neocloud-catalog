// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package airunway exports the catalog to AI Runway: vLLM recipes (served from
// VLLM_RECIPES_BASE_URL) and ModelDeployment manifests with POST /api/deployments bodies.
package airunway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

const (
	FormatRecipes     = "airunway-recipes"
	FormatDeployments = "airunway-deployments"
)

// Skip reasons.
const (
	SkipNotVLLM        = "recipes resolver is vLLM only"
	SkipRecipeNoHF     = "recipes need a Hugging Face org/model repo"
	SkipRepoTaken      = "one recipe per Hugging Face repo"
	SkipRecipeTopology = "recipes resolve to one Direct vLLM server"
	SkipBackend        = "backend AI Runway cannot run"
	// SkipLlamaCpp: KAITO's template passes no engine flags, so the pinned llama-server cannot run.
	SkipLlamaCpp  = "KAITO runs llama.cpp only as an AIKit runner"
	SkipNoHF      = "ModelDeployment source is huggingface or custom only"
	SkipNoFit     = "no whole-GPU fit to size the GPU count"
	SkipMultiNode = "AI Runway has no multi-node deployments"
	// SkipDisaggregated: Dynamo gives both roles one set of engine args.
	SkipDisaggregated = "topology needs Dynamo disaggregated mode"
)

// gpuVendor is the only vendor AI Runway's deployment API requests (nvidia.com/gpu).
const gpuVendor = "nvidia"

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
		return []string{SkipNotVLLM, SkipRecipeNoHF, SkipRepoTaken, SkipRecipeTopology, SkipNoFit}
	case FormatDeployments:
		return []string{SkipBackend, SkipLlamaCpp, SkipNoHF, SkipNoFit, SkipMultiNode, SkipDisaggregated}
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

type target struct {
	adapters.Ref
	source  catalog.ModelSource
	release catalog.BackendVersion
	fit     catalog.ModelFit
	draft   *catalog.DraftModel
}

// resolve returns a preset's target, or the skip reason (noHF without a source).
func resolve(snap catalog.CatalogSnapshot, r adapters.Ref, opts adapters.Options, noHF string) (target, string, error) {
	src, ok := r.Source(catalog.SourceHuggingFace)
	if !ok {
		return target{}, noHF, nil
	}
	rel, ok := adapters.BackendVersion(snap, r.Preset.Backend, r.Preset.BackendVersion)
	if !ok {
		return target{}, "", fmt.Errorf("%s/%s: backend %s %s is not in the snapshot", r.Model.Slug, r.Preset.Name, r.Preset.Backend, r.Preset.BackendVersion)
	}
	vendors := slices.DeleteFunc(adapters.Vendors(r.Preset, rel), func(v string) bool { return v != gpuVendor })
	fit, ok := adapters.ReferenceFit(snap, r, vendors, opts)
	if !ok {
		return target{}, SkipNoFit, nil
	}
	return target{Ref: r, source: src, release: rel, fit: fit, draft: adapters.Draft(snap, r.Preset)}, "", nil
}

func skip(e *adapters.Export, r adapters.Ref, reason string) {
	e.Skipped = append(e.Skipped, adapters.Skip{PresetKey: r.Key(), Reason: reason})
}

// encode writes indented JSON, which kubectl also reads as YAML.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(v)
	return b.Bytes(), err
}
