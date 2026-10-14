// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package airunway

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

// RequiredProviderAnnotation names the AI Runway provider spec.provider.name pins.
const RequiredProviderAnnotation = "neocloud.io/required-provider"

// HFTokenSecret is where AI Runway stores the HF token (key HF_TOKEN); gated models reference it.
const HFTokenSecret = "hf-token-secret"

type runtime struct {
	provider string
	// Dynamo runs python3 -m dynamo.<engine> in the image, which upstream SGLang
	// lacks, so SGLang keeps Dynamo's sglang-runtime image.
	pinnedImage bool
	// derivesTP: the provider sets tensor parallelism from the GPU count.
	derivesTP     bool
	disaggregated bool
}

var runtimes = map[string]runtime{
	"vllm":   {provider: "vllm", pinnedImage: true, derivesTP: true},
	"sglang": {provider: "dynamo", disaggregated: true},
}

// modelDeployment mirrors controller/api/v1alpha1/modeldeployment_types.go.
type modelDeployment struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   metadata `json:"metadata"`
	Spec       mdSpec   `json:"spec"`
}

type metadata struct {
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations"`
}

type mdSpec struct {
	Model     mdModel      `json:"model"`
	Provider  mdProvider   `json:"provider"`
	Engine    mdEngine     `json:"engine"`
	Serving   *mdServing   `json:"serving,omitempty"`
	Scaling   *mdScaling   `json:"scaling,omitempty"`
	Resources *mdResources `json:"resources,omitempty"`
	Env       []mdEnv      `json:"env,omitempty"`
	Secrets   *mdSecrets   `json:"secrets,omitempty"`
}

type mdModel struct {
	ID     string `json:"id"`
	Source string `json:"source"`
}

type mdProvider struct {
	Name string `json:"name"`
}

type mdEngine struct {
	Type          string            `json:"type"`
	Image         string            `json:"image,omitempty"`
	ContextLength int               `json:"contextLength"`
	Args          map[string]string `json:"args,omitempty"`
	ExtraArgs     []string          `json:"extraArgs,omitempty"`
}

type mdServing struct {
	Mode string `json:"mode"`
}

type mdScaling struct {
	Prefill *mdComponent `json:"prefill"`
	Decode  *mdComponent `json:"decode"`
}

type mdComponent struct {
	Replicas int   `json:"replicas"`
	GPU      mdGPU `json:"gpu"`
}

type mdResources struct {
	GPU mdGPU `json:"gpu"`
}

type mdGPU struct {
	Count int `json:"count"`
}

type mdEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type mdSecrets struct {
	HuggingFaceToken string `json:"huggingFaceToken"`
}

// apiBody mirrors createDeploymentSchema in backend/src/routes/deployments.ts.
type apiBody struct {
	Name            string            `json:"name"`
	ModelID         string            `json:"modelId"`
	Engine          string            `json:"engine"`
	Mode            string            `json:"mode,omitempty"`
	Provider        string            `json:"provider"`
	ImageRef        string            `json:"imageRef,omitempty"`
	ContextLength   int               `json:"contextLength"`
	Resources       *apiResources     `json:"resources,omitempty"`
	PrefillGpus     int               `json:"prefillGpus,omitempty"`
	DecodeGpus      int               `json:"decodeGpus,omitempty"`
	EngineArgs      map[string]string `json:"engineArgs,omitempty"`
	EngineExtraArgs []string          `json:"engineExtraArgs,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	HFTokenSecret   string            `json:"hfTokenSecret,omitempty"`
}

type apiResources struct {
	GPU int `json:"gpu"`
}

func exportDeployments(snap catalog.CatalogSnapshot, opts adapters.Options) (adapters.Export, error) {
	var e adapters.Export
	names := map[string]adapters.PresetKey{}
	for _, r := range adapters.Presets(snap) {
		rt, ok := runtimes[r.Preset.Backend]
		top := r.Preset.Topology
		switch {
		case r.Preset.Backend == "llamacpp":
			skip(&e, r, SkipLlamaCpp)
			continue
		case !ok:
			skip(&e, r, SkipBackend)
			continue
		case top.Kind == "multi-node":
			skip(&e, r, SkipMultiNode)
			continue
		case top.Kind == "disaggregated" && !rt.disaggregated:
			skip(&e, r, SkipDisaggregated)
			continue
		}
		t, reason, err := resolve(snap, r, opts, SkipNoHF)
		if err != nil {
			return adapters.Export{}, err
		}
		if reason != "" {
			skip(&e, r, reason)
			continue
		}
		args, extra, ok, err := render(t, rt)
		if err != nil {
			return adapters.Export{}, err
		}
		if !ok {
			skip(&e, r, SkipDisaggregated)
			continue
		}
		name, err := dnsLabel(r.Model.Slug + "-" + r.Preset.Name)
		if err != nil {
			return adapters.Export{}, err
		}
		if prev, dup := names[name]; dup {
			return adapters.Export{}, fmt.Errorf("%s/%s and %s/%s both name the deployment %s", prev.Model, prev.Preset, r.Model.Slug, r.Preset.Name, name)
		}
		names[name] = r.Key()

		image := ""
		if rt.pinnedImage {
			image = t.release.Images[gpuVendor]
		}
		ctx := cmp.Or(r.Preset.Context, r.Model.DefaultContext)
		md := modelDeployment{
			APIVersion: "airunway.ai/v1alpha1",
			Kind:       "ModelDeployment",
			Metadata: metadata{
				Name:        name,
				Annotations: map[string]string{RequiredProviderAnnotation: rt.provider},
			},
			Spec: mdSpec{
				Model:    mdModel{ID: t.source.Repo, Source: catalog.SourceHuggingFace},
				Provider: mdProvider{Name: rt.provider},
				Engine: mdEngine{
					Type:          r.Preset.Backend,
					Image:         image,
					ContextLength: ctx,
					Args:          args,
					ExtraArgs:     extra,
				},
			},
		}
		body := apiBody{
			Name:            name,
			ModelID:         t.source.Repo,
			Engine:          r.Preset.Backend,
			Provider:        rt.provider,
			ImageRef:        image,
			ContextLength:   ctx,
			EngineArgs:      args,
			EngineExtraArgs: extra,
		}
		if top.Kind == "disaggregated" {
			md.Spec.Serving = &mdServing{Mode: "disaggregated"}
			md.Spec.Scaling = &mdScaling{
				Prefill: &mdComponent{Replicas: 1, GPU: mdGPU{Count: top.Prefill}},
				Decode:  &mdComponent{Replicas: 1, GPU: mdGPU{Count: top.Decode}},
			}
			body.Mode, body.PrefillGpus, body.DecodeGpus = "disaggregated", top.Prefill, top.Decode
		} else {
			md.Spec.Resources = &mdResources{GPU: mdGPU{Count: t.fit.GPUCount}}
			body.Resources = &apiResources{GPU: t.fit.GPUCount}
		}
		for _, k := range slices.Sorted(maps.Keys(r.Preset.Env)) {
			md.Spec.Env = append(md.Spec.Env, mdEnv{Name: k, Value: r.Preset.Env[k]})
		}
		body.Env = r.Preset.Env
		if r.Model.Gated {
			md.Spec.Secrets = &mdSecrets{HuggingFaceToken: HFTokenSecret}
			body.HFTokenSecret = HFTokenSecret
		}
		dir := r.Model.Slug + "/" + r.Preset.Name
		for path, v := range map[string]any{"deployments/" + dir + ".yaml": md, "api/" + dir + ".json": body} {
			b, err := encode(v)
			if err != nil {
				return adapters.Export{}, err
			}
			e.Artifacts = append(e.Artifacts, adapters.Artifact{Path: path, Body: b})
		}
		e.Exported = append(e.Exported, r.Key())
	}
	slices.SortFunc(e.Artifacts, func(a, b adapters.Artifact) int { return strings.Compare(a.Path, b.Path) })
	return e, nil
}

// render returns engine args without the context (contextLength carries it). Dynamo
// gives both disaggregated workers one set and adds --disaggregation-mode itself,
// so ok is false when the roles differ.
func render(t target, rt runtime) (args map[string]string, extra []string, ok bool, err error) {
	p := t.Preset
	p.Context = 0
	fit := t.fit
	if rt.derivesTP {
		fit = catalog.ModelFit{}
	}
	if p.Topology.Kind != "disaggregated" {
		tokens, err := catalog.Render(t.Model, p, t.release, fit, t.draft, gpuVendor)
		if err != nil {
			return nil, nil, false, err
		}
		args, extra := engineArgs(tokens)
		return args, extra, true, nil
	}
	var roleArgs [2]map[string]string
	var roleExtra [2][]string
	for i, role := range []string{"prefill", "decode"} {
		tokens, err := catalog.RenderRole(t.Model, p, t.release, fit, t.draft, gpuVendor, role, 0)
		if err != nil {
			return nil, nil, false, err
		}
		roleArgs[i], roleExtra[i] = engineArgs(tokens)
		delete(roleArgs[i], "disaggregation-mode")
	}
	ok = maps.Equal(roleArgs[0], roleArgs[1]) && slices.Equal(roleExtra[0], roleExtra[1])
	return roleArgs[0], roleExtra[0], ok, nil
}

// dnsLabel turns s into a webhook-accepted name (no dots) that fits derived Service names.
func dnsLabel(s string) (string, error) {
	name := strings.Trim(strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' || '0' <= r && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(s)), "-")
	if name == "" || len(name) > 63 {
		return "", fmt.Errorf("%q makes no DNS label of 1-63 characters", s)
	}
	return name, nil
}

var argKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// engineArgs maps flags to spec.engine.args ("--key value", "--key" if empty). What a
// map cannot hold stays in extraArgs, in order; the webhook rejects a key in both.
func engineArgs(tokens []string) (map[string]string, []string) {
	args := map[string]string{}
	for i := 0; i < len(tokens); i++ {
		key, value, inline := strings.Cut(strings.TrimPrefix(tokens[i], "--"), "=")
		_, dup := args[key]
		if !strings.HasPrefix(tokens[i], "--") || !argKey.MatchString(key) || dup || inline && value == "" {
			return nil, tokens
		}
		if !inline && i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "--") {
			i++
			value = tokens[i]
		}
		args[key] = value
	}
	return args, nil
}
