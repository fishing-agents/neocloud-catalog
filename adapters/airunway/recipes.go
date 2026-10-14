// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package airunway

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

// Recipe fields AI Runway reads (vllmRecipeResolver.ts), in recipes.vllm.ai's layout.
type recipe struct {
	HFID               string             `json:"hf_id"`
	Meta               recipeMeta         `json:"meta"`
	RecommendedCommand recommendedCommand `json:"recommended_command"`
	Model              recipeModel        `json:"model"`
	Variants           map[string]variant `json:"variants"`
}

type recipeMeta struct {
	Title       string            `json:"title"`
	Provider    string            `json:"provider"`
	Hardware    map[string]string `json:"hardware"`
	DerivedFrom string            `json:"derived_from,omitempty"`
	Variant     string            `json:"variant,omitempty"`
}

type recommendedCommand struct {
	Hardware        string          `json:"hardware"`
	Variant         string          `json:"variant"`
	DockerImage     string          `json:"docker_image"`
	Argv            []string        `json:"argv"`
	HardwareProfile hardwareProfile `json:"hardware_profile"`
}

type hardwareProfile struct {
	GPUCount int `json:"gpu_count"`
}

type recipeModel struct {
	ModelID        string `json:"model_id"`
	MinVLLMVersion string `json:"min_vllm_version"`
	ParameterCount string `json:"parameter_count"`
	ContextLength  int    `json:"context_length"`
}

type variant struct {
	ModelID       string  `json:"model_id,omitempty"`
	Precision     string  `json:"precision"`
	VRAMMinimumGB float64 `json:"vram_minimum_gb"`
	JSON          string  `json:"json,omitempty"`
}

type indexEntry struct {
	HFID        string `json:"hf_id"`
	Title       string `json:"title"`
	Provider    string `json:"provider"`
	JSON        string `json:"json"`
	DerivedFrom string `json:"derived_from,omitempty"`
}

// fitStatus marks fit-matrix families in meta.hardware; upstream reserves
// "verified" for GPUs tested end to end.
const fitStatus = "supported"

// exportRecipes writes one recipe per HF repo, as AI Runway fetches <hf_id>.json.
// A model's first vLLM preset is the base; others derive from it (meta.derived_from).
func exportRecipes(snap catalog.CatalogSnapshot, opts adapters.Options) (adapters.Export, error) {
	var e adapters.Export
	var models [][]target // per model, its recipes; the first is the base
	taken := map[string]bool{}
	for _, r := range adapters.Presets(snap) {
		if r.Preset.Backend != "vllm" {
			skip(&e, r, SkipNotVLLM)
			continue
		}
		if k := r.Preset.Topology.Kind; k != "" && k != "single" {
			skip(&e, r, SkipRecipeTopology)
			continue
		}
		t, reason, err := resolve(snap, r, opts, SkipRecipeNoHF)
		if err != nil {
			return adapters.Export{}, err
		}
		if reason == "" && strings.Count(t.source.Repo, "/") != 1 {
			reason = SkipRecipeNoHF
		}
		if reason == "" && taken[t.source.Repo] {
			reason = SkipRepoTaken
		}
		if reason != "" {
			skip(&e, r, reason)
			continue
		}
		taken[t.source.Repo] = true
		e.Exported = append(e.Exported, r.Key())
		if n := len(models); n > 0 && models[n-1][0].Model.Slug == r.Model.Slug {
			models[n-1] = append(models[n-1], t)
		} else {
			models = append(models, []target{t})
		}
	}

	index := []indexEntry{}
	var files []adapters.Artifact
	for _, group := range models {
		for _, t := range group {
			rc, err := newRecipe(snap, t, group)
			if err != nil {
				return adapters.Export{}, err
			}
			body, err := encode(rc)
			if err != nil {
				return adapters.Export{}, err
			}
			files = append(files, adapters.Artifact{Path: recipePath(t), Body: body})
			index = append(index, indexEntry{
				HFID:        rc.HFID,
				Title:       rc.Meta.Title,
				Provider:    rc.Meta.Provider,
				JSON:        recipePath(t),
				DerivedFrom: rc.Meta.DerivedFrom,
			})
		}
	}
	body, err := encode(index)
	if err != nil {
		return adapters.Export{}, err
	}
	e.Artifacts = append([]adapters.Artifact{{Path: "models.json", Body: body}}, files...)
	return e, nil
}

func newRecipe(snap catalog.CatalogSnapshot, t target, group []target) (recipe, error) {
	base := group[0]
	org, _, _ := strings.Cut(base.source.Repo, "/")
	// The Direct vLLM provider derives tensor parallelism from the GPU count.
	args, err := catalog.Render(t.Model, t.Preset, t.release, catalog.ModelFit{}, t.draft, gpuVendor)
	if err != nil {
		return recipe{}, err
	}
	rc := recipe{
		HFID: t.source.Repo,
		Meta: recipeMeta{Title: t.Model.Name, Provider: org, Hardware: map[string]string{}},
		RecommendedCommand: recommendedCommand{
			Hardware:        hardwareID(t.fit.DeviceFamily),
			Variant:         "default",
			DockerImage:     t.release.Images[gpuVendor],
			Argv:            append([]string{"vllm", "serve", t.source.Repo}, args...),
			HardwareProfile: hardwareProfile{GPUCount: t.fit.GPUCount},
		},
		Model: recipeModel{
			ModelID:        t.source.Repo,
			MinVLLMVersion: strings.TrimPrefix(t.release.Version, "v"),
			ParameterCount: strconv.FormatFloat(t.Model.ParamsB, 'f', -1, 64) + "B",
			ContextLength:  t.Model.ContextMax,
		},
		Variants: map[string]variant{"default": newVariant(t)},
	}
	for _, f := range snap.Fits {
		nvidia := slices.ContainsFunc(snap.Families, func(fam catalog.Family) bool { return fam.Name == f.DeviceFamily && fam.Vendor == gpuVendor })
		if f.ModelSlug == t.Model.Slug && f.Quantization == t.Preset.Quantization && f.KVCacheDType == t.fit.KVCacheDType && f.Profile == nil && f.Nodes <= 1 && nvidia {
			rc.Meta.Hardware[hardwareID(f.DeviceFamily)] = fitStatus
		}
	}
	if t.source.Repo != base.source.Repo {
		rc.Meta.DerivedFrom = base.source.Repo
		rc.Meta.Variant = t.Preset.Quantization
		rc.RecommendedCommand.Variant = t.Preset.Quantization
		return rc, nil
	}
	for _, d := range group[1:] {
		v := newVariant(d)
		v.ModelID = d.source.Repo
		v.JSON = recipePath(d)
		rc.Variants[d.Preset.Quantization] = v
	}
	return rc, nil
}

// newVariant sizes a checkpoint at its served context, rounded up to 0.01 GB.
func newVariant(t target) variant {
	vram, _ := catalog.VRAMRequired(t.Model, t.Preset.Quantization, cmp.Or(t.Preset.Context, t.Model.DefaultContext))
	return variant{Precision: t.Preset.Quantization, VRAMMinimumGB: math.Ceil(vram*100) / 100}
}

// recipePath is <org>/<model>.json, where the recipes client fetches it.
func recipePath(t target) string { return t.source.Repo + ".json" }

// hardwareID spells a family as recipes name hardware (h100, rtx_4090).
func hardwareID(family string) string {
	return strings.ToLower(strings.ReplaceAll(family, "-", "_"))
}
