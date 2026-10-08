package airunway

import (
	"math"
	"strconv"
	"strings"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

// The recipe fields AI Runway reads (backend/src/services/vllmRecipeResolver.ts,
// vllmRecipesClient.ts), in the layout of recipes.vllm.ai.
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

// fitStatus marks a family the fit matrix covers in meta.hardware. Upstream
// reserves "verified" for GPUs tested end to end; a fit is computed.
const fitStatus = "supported"

// exportRecipes writes one recipe per Hugging Face repo, because AI Runway
// fetches <hf_id>.json and its UI matches a recipe by hf_id: a model's first
// vLLM preset is its base recipe, every other one a recipe derived from it
// (meta.derived_from) and listed among the base's variants.
func exportRecipes(snap catalog.CatalogSnapshot, opts adapters.Options) (adapters.Export, error) {
	var e adapters.Export
	var models [][]target // per model, its recipes; the first is the base
	taken := map[string]bool{}
	for _, r := range adapters.Presets(snap) {
		if r.Preset.Backend != "vllm" {
			skip(&e, r, SkipNotVLLM)
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
			rc := newRecipe(snap, t, group)
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

func newRecipe(snap catalog.CatalogSnapshot, t target, group []target) recipe {
	base := group[0]
	org, _, _ := strings.Cut(base.source.Repo, "/")
	ctx, _ := adapters.ContextArg("vllm", t.Model.DefaultContext)
	rc := recipe{
		HFID: t.source.Repo,
		Meta: recipeMeta{Title: t.Model.Name, Provider: org, Hardware: map[string]string{}},
		RecommendedCommand: recommendedCommand{
			Hardware:        hardwareID(t.fit.DeviceFamily),
			Variant:         "default",
			DockerImage:     t.release.Image,
			Argv:            append(append([]string{"vllm", "serve", t.source.Repo}, t.Preset.Args...), ctx),
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
		if f.ModelSlug == t.Model.Slug && f.Quantization == t.Preset.Quantization && f.Profile == nil {
			rc.Meta.Hardware[hardwareID(f.DeviceFamily)] = fitStatus
		}
	}
	if t.source.Repo != base.source.Repo {
		rc.Meta.DerivedFrom = base.source.Repo
		rc.Meta.Variant = t.Preset.Quantization
		rc.RecommendedCommand.Variant = t.Preset.Quantization
		return rc
	}
	for _, d := range group[1:] {
		v := newVariant(d)
		v.ModelID = d.source.Repo
		v.JSON = recipePath(d)
		rc.Variants[d.Preset.Quantization] = v
	}
	return rc
}

// newVariant sizes a checkpoint at the context its command serves, rounded up
// to 0.01 GB like the fit matrix.
func newVariant(t target) variant {
	vram, _ := catalog.VRAMRequired(t.Model, t.Preset.Quantization, t.Model.DefaultContext)
	return variant{Precision: t.Preset.Quantization, VRAMMinimumGB: math.Ceil(vram*100) / 100}
}

// recipePath is where the recipes client fetches a repo's recipe:
// <base>/<org>/<model>.json.
func recipePath(t target) string { return t.source.Repo + ".json" }

// hardwareID spells a device family the way recipes name hardware (h100,
// rtx_4090).
func hardwareID(family string) string {
	return strings.ToLower(strings.ReplaceAll(family, "-", "_"))
}
