package airunway

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

// RequiredProviderAnnotation names the AI Runway provider a manifest runs on
// and spec.provider.name pins.
const RequiredProviderAnnotation = "neocloud.ai/required-provider"

// HFTokenSecret is the secret AI Runway's settings store a Hugging Face token
// in (key HF_TOKEN); manifests of gated models reference it.
const HFTokenSecret = "hf-token-secret"

// runtime is how AI Runway runs a catalog backend.
type runtime struct {
	provider string
	// pinnedImage: the provider runs the backend's own image. Dynamo starts
	// python3 -m dynamo.<engine> and its frontend inside spec.engine.image
	// (providers/dynamo/transformer.go), which the upstream SGLang image
	// lacks, so SGLang keeps Dynamo's sglang-runtime image.
	pinnedImage bool
	// tpFlag is set when the provider does not derive tensor parallelism from
	// resources.gpu.count (the vLLM provider does, Dynamo does not).
	tpFlag string
}

var runtimes = map[string]runtime{
	"vllm":   {provider: "vllm", pinnedImage: true},
	"sglang": {provider: "dynamo", tpFlag: "--tp-size"},
}

// The ModelDeployment fields set (controller/api/v1alpha1/modeldeployment_types.go).
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
	Model     mdModel     `json:"model"`
	Provider  mdProvider  `json:"provider"`
	Engine    mdEngine    `json:"engine"`
	Resources mdResources `json:"resources"`
	Secrets   *mdSecrets  `json:"secrets,omitempty"`
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

type mdResources struct {
	GPU mdGPU `json:"gpu"`
}

type mdGPU struct {
	Count int `json:"count"`
}

type mdSecrets struct {
	HuggingFaceToken string `json:"huggingFaceToken"`
}

// The POST /api/deployments body (createDeploymentSchema in
// backend/src/routes/deployments.ts).
type apiBody struct {
	Name            string            `json:"name"`
	ModelID         string            `json:"modelId"`
	Engine          string            `json:"engine"`
	Provider        string            `json:"provider"`
	ImageRef        string            `json:"imageRef,omitempty"`
	ContextLength   int               `json:"contextLength"`
	Resources       apiResources      `json:"resources"`
	EngineArgs      map[string]string `json:"engineArgs,omitempty"`
	EngineExtraArgs []string          `json:"engineExtraArgs,omitempty"`
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
		if !ok {
			skip(&e, r, SkipBackend)
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
		name, err := dnsLabel(r.Model.Slug + "-" + r.Preset.Name)
		if err != nil {
			return adapters.Export{}, err
		}
		if prev, dup := names[name]; dup {
			return adapters.Export{}, fmt.Errorf("%s/%s and %s/%s both name the deployment %s", prev.Model, prev.Preset, r.Model.Slug, r.Preset.Name, name)
		}
		names[name] = r.Key()

		tokens := r.Preset.Args
		if rt.tpFlag != "" && t.fit.GPUCount > 1 && !setsTP(tokens) {
			tokens = append(slices.Clip(tokens), rt.tpFlag, strconv.Itoa(t.fit.GPUCount))
		}
		args, extra := engineArgs(tokens)
		image := ""
		if rt.pinnedImage {
			image = t.release.Image
		}
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
					ContextLength: r.Model.DefaultContext,
					Args:          args,
					ExtraArgs:     extra,
				},
				Resources: mdResources{GPU: mdGPU{Count: t.fit.GPUCount}},
			},
		}
		body := apiBody{
			Name:            name,
			ModelID:         t.source.Repo,
			Engine:          r.Preset.Backend,
			Provider:        rt.provider,
			ImageRef:        image,
			ContextLength:   r.Model.DefaultContext,
			Resources:       apiResources{GPU: t.fit.GPUCount},
			EngineArgs:      args,
			EngineExtraArgs: extra,
		}
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

// dnsLabel turns s into a name the webhook accepts (no dots) that also fits the
// Service and volume names derived from it.
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

// argKey is a key AI Runway accepts in spec.engine.args.
var argKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// engineArgs maps flags to spec.engine.args, which providers render as
// "--key value", or "--key" for an empty value. Arguments a map cannot hold
// (a positional token, a repeated key, an empty "--key=") all stay in
// extraArgs, in order, since the webhook rejects a key in both.
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

func setsTP(tokens []string) bool {
	return slices.ContainsFunc(tokens, func(t string) bool {
		key, _, _ := strings.Cut(t, "=")
		return key == "--tp-size" || key == "--tp" || key == "--tensor-parallel-size"
	})
}
