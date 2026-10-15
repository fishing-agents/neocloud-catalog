// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package huggingface

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	catalog "github.com/fishman/neocloud-catalog"
)

// ErrNotFound reports a repository or revision the Hub does not show the token: missing or private.
var ErrNotFound = errors.New("repository not found or not readable")

// Repository is one Hub repository at a commit, read for an import or an inspection.
type Repository struct {
	Repo, Commit, URL string
	Private, Gated    bool
	License           string
	PipelineTag       string
	Tags              []string
	Card              map[string]any
	CreatedAt         time.Time
	// Files is every file of the commit; Params the safetensors parameter count (0 unknown).
	Files  []File
	Params int64

	tree        []entry
	manifest    []catalog.ModelFile
	weights     int64
	manifestErr error
	config      *config
	extra       configExtra
	configErr   error
}

// File is a repository file.
type File struct {
	Path string
	Size int64
}

// Quantization is a checkpoint a repository holds: its weights' (float, fp8 or another method's), or
// one GGUF quantization's files.
type Quantization struct {
	Name, Kind string
	Bytes      int64
	Files      []string
}

type repoInfo struct {
	SHA         string         `json:"sha"`
	Private     bool           `json:"private"`
	Gated       any            `json:"gated"` // false, "auto" or "manual"
	PipelineTag string         `json:"pipeline_tag"`
	Tags        []string       `json:"tags"`
	Card        map[string]any `json:"cardData"`
	CreatedAt   time.Time      `json:"createdAt"`
	Safetensors *struct {
		Total int64 `json:"total"`
	} `json:"safetensors"`
}

// configExtra is what config.json says of the weights' format.
type configExtra struct {
	ModelType  string `json:"model_type"`
	TorchDType string `json:"torch_dtype"`
	DType      string `json:"dtype"` // transformers 4.56 renamed torch_dtype
	Quant      *struct {
		Method       string `json:"quant_method"`
		ConfigGroups map[string]struct {
			Weights *struct {
				Type    string `json:"type"`
				NumBits int    `json:"num_bits"`
			} `json:"weights"`
		} `json:"config_groups"`
	} `json:"quantization_config"`
}

// Endpoint returns the Hub the importer reads.
func (i *Importer) Endpoint() string { return i.endpoint }

// with returns a client reading with token instead of the importer's.
func (i *Importer) with(token string) *client {
	cp := *i
	cp.token = token
	return &client{Importer: &cp}
}

// Read reads repo at revision (the default branch if empty), with token if set, else the importer's.
// A repository the Hub refuses to show is ErrNotFound; unreadable files or config.json are left to
// Template and Quantizations.
func (i *Importer) Read(ctx context.Context, repo, revision, token string) (*Repository, error) {
	c := i.with(cmp.Or(token, i.token))
	api := c.endpoint + "/api/models/" + repo
	var inf repoInfo
	if err := c.getJSON(ctx, api+"/revision/"+url.PathEscape(cmp.Or(revision, "main")), &inf); err != nil {
		if he := (*httpError)(nil); errors.As(err, &he) && slices.Contains([]int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}, he.code) {
			return nil, fmt.Errorf("%s at %s: %w: %w", repo, cmp.Or(revision, "main"), ErrNotFound, err)
		}
		return nil, err
	}
	if len(inf.SHA) != 40 {
		return nil, fmt.Errorf("%s at %s: the Hub names no commit", repo, cmp.Or(revision, "main"))
	}
	r := &Repository{Repo: repo, Commit: inf.SHA, URL: c.endpoint + "/" + repo, Private: inf.Private, Gated: inf.Gated != nil && inf.Gated != false,
		PipelineTag: inf.PipelineTag, Tags: inf.Tags, Card: inf.Card, CreatedAt: inf.CreatedAt}
	if l, ok := inf.Card["license"].(string); ok {
		r.License = l
	}
	if inf.Safetensors != nil {
		r.Params = inf.Safetensors.Total
	}
	tree, err := c.tree(ctx, api+"/tree/"+inf.SHA+"?recursive=1")
	if err != nil {
		return nil, err
	}
	for _, e := range tree {
		if e.Type == "file" {
			r.tree = append(r.tree, e)
			r.Files = append(r.Files, File{e.Path, e.Size})
		}
	}
	r.manifest, r.weights, r.manifestErr = manifest(r.tree, false, nil)
	if errors.Is(r.manifestErr, errDigestHidden) {
		r.manifestErr = c.gated(r.manifestErr)
	}
	body, _, err := c.get(ctx, c.endpoint+"/"+repo+"/resolve/"+inf.SHA+"/config.json")
	if err == nil {
		var cfg config
		if cfg, err = parseConfig(body); err == nil {
			r.config = &cfg
			err = json.Unmarshal(body, &r.extra)
		}
	}
	r.configErr = err
	return r, nil
}

// Hit is a search result.
type Hit struct {
	Repo        string `json:"id"`
	PipelineTag string `json:"pipeline_tag"`
	Library     string `json:"library_name"`
	Downloads   int    `json:"downloads"`
	Likes       int    `json:"likes"`
}

// Search lists up to limit repositories matching q, most downloaded first. It reads anonymously, so no
// private repository of the token's account shows.
func (i *Importer) Search(ctx context.Context, q string, limit int) ([]Hit, error) {
	hits := []Hit{}
	query := url.Values{"search": {q}, "limit": {strconv.Itoa(limit)}, "sort": {"downloads"}, "direction": {"-1"}}
	return hits, i.with("").getJSON(ctx, i.endpoint+"/api/models?"+query.Encode(), &hits)
}

// TotalBytes is the size of every file.
func (r *Repository) TotalBytes() int64 {
	var n int64
	for _, f := range r.Files {
		n += f.Size
	}
	return n
}

// WeightsBytes is the size of the weights an engine loads (one format), else of the GGUF files.
func (r *Repository) WeightsBytes() int64 {
	if r.weights > 0 {
		return r.weights
	}
	var n int64
	for _, e := range r.tree {
		if path.Ext(e.Path) == ".gguf" {
			n += e.Size
		}
	}
	return n
}

// Architecture is config.json's, and its maximum context; zero if unreadable.
func (r *Repository) Architecture() (catalog.Architecture, int) {
	if r.config == nil {
		return catalog.Architecture{}, 0
	}
	return r.config.architecture(), r.config.MaxPositions
}

// Task is the catalog task of the repository's pipeline tag, or "".
func (r *Repository) Task() string {
	switch r.PipelineTag {
	case "text-generation":
		if slices.Contains(r.Tags, "conversational") {
			return "chat"
		}
		return "completion"
	case "image-text-to-text":
		return "vision-chat"
	case "feature-extraction", "sentence-similarity":
		return "embedding"
	case "text-ranking":
		return "rerank"
	}
	return ""
}

var ggufQuant = regexp.MustCompile(`(?i)[-_.]((?:I?Q\d[A-Z0-9_]*)|BF16|F16|F32)(?:-\d{5}-of-\d{5})?\.gguf$`)

// Quantizations are the checkpoints the repository holds: its weights' (when config.json is readable),
// then each GGUF quantization by name.
func (r *Repository) Quantizations() []Quantization {
	var out []Quantization
	if q, ok := r.weightsQuantization(); ok {
		out = append(out, q)
	}
	gguf := map[string]*Quantization{}
	for _, e := range r.tree {
		m := ggufQuant.FindStringSubmatch(e.Path)
		if m == nil {
			continue
		}
		name := strings.ToUpper(m[1])
		q := gguf[name]
		if q == nil {
			q = &Quantization{Name: name, Kind: "gguf"}
			gguf[name] = q
		}
		q.Bytes += e.Size
		q.Files = append(q.Files, e.Path)
	}
	for _, name := range slices.Sorted(maps.Keys(gguf)) {
		out = append(out, *gguf[name])
	}
	return out
}

// weightsQuantization names the weights' quantization from config.json: float by its dtype, fp8, or
// another quantization method (named and kinded by it).
func (r *Repository) weightsQuantization() (Quantization, bool) {
	if r.config == nil || r.weights == 0 {
		return Quantization{}, false
	}
	q := Quantization{Bytes: r.weights}
	format := ""
	for _, ext := range weightFormats {
		if slices.ContainsFunc(r.tree, func(e entry) bool { return path.Ext(e.Path) == ext }) {
			format = ext
			break
		}
	}
	for _, e := range r.tree {
		if path.Ext(e.Path) == format && e.Size > 0 {
			q.Files = append(q.Files, e.Path)
		}
	}
	switch qc := r.extra.Quant; {
	case qc == nil:
		q.Kind = "float"
		q.Name = map[string]string{"bfloat16": "bf16", "float16": "fp16", "float32": "fp32"}[cmp.Or(r.extra.DType, r.extra.TorchDType)]
		if q.Name == "" {
			q.Name = cmp.Or(r.extra.DType, r.extra.TorchDType, "bf16")
		}
	case qc.Method == "fp8":
		q.Name, q.Kind = "fp8", "fp8"
	default:
		q.Name, q.Kind = qc.Method, qc.Method
		for _, g := range qc.ConfigGroups {
			if qc.Method == "compressed-tensors" && g.Weights != nil && g.Weights.Type == "float" && g.Weights.NumBits == 8 {
				q.Name, q.Kind = "fp8", "fp8"
			}
		}
	}
	return q, true
}

// Slug is the catalog slug of repo: owner--name lower-cased, unique as the Hub refuses "--" in ids.
func Slug(repo string) string { return strings.ToLower(strings.ReplaceAll(repo, "/", "--")) }

// defaultContext caps a model's default context, as the seed models have it.
const defaultContext = 8192

// Template is the catalog model of the repository: its weights' quantization with every file's digest,
// and a standard preset on each backend whose newest version (last listed) runs that quantization's
// kind. It refuses what the catalog cannot serve: an unreadable config.json or manifest (gated without
// a token), an unknown parameter count, GGUF-only repositories (they hold every quantization), a
// quantization no backend runs and a pipeline tag no catalog task serves.
func (r *Repository) Template(backends []catalog.Backend) (catalog.ModelTemplate, error) {
	switch {
	case r.weights == 0 && slices.ContainsFunc(r.tree, func(e entry) bool { return path.Ext(e.Path) == ".gguf" }):
		return catalog.ModelTemplate{}, errors.New("a GGUF repository holds every quantization: curate the one to serve as a local model")
	case r.weights == 0:
		return catalog.ModelTemplate{}, fmt.Errorf("no weights (%s)", strings.Join(weightFormats, ", "))
	case r.configErr != nil:
		return catalog.ModelTemplate{}, r.configErr
	case r.manifestErr != nil:
		return catalog.ModelTemplate{}, r.manifestErr
	case r.Params <= 0:
		return catalog.ModelTemplate{}, errors.New("parameter count unknown: the Hub has no safetensors metadata")
	}
	q, _ := r.weightsQuantization()
	task := r.Task()
	if task == "" {
		return catalog.ModelTemplate{}, fmt.Errorf("pipeline tag %q: the catalog serves chat, completion, embedding, rerank and vision-chat models", r.PipelineTag)
	}
	active, err := r.config.active(r.Params)
	if err != nil {
		return catalog.ModelTemplate{}, err
	}
	quant := catalog.Quantization{Name: q.Name, Kind: q.Kind, RequiresDType: map[string]string{"bf16": "bf16", "fp8": "fp8"}[q.Name],
		WeightsGB: tenths(r.weights), Sources: []catalog.ModelSource{{Type: catalog.SourceHuggingFace, Repo: r.Repo, Revision: r.Commit, Files: r.manifest}}}
	var presets []catalog.Preset
	for _, b := range backends {
		for j := len(b.Versions) - 1; j >= 0; j-- {
			if _, ok := b.Versions[j].Quantizations[q.Kind]; ok {
				presets = append(presets, catalog.Preset{Name: b.Name + "-" + q.Name, Quantization: q.Name, Backend: b.Name,
					BackendVersion: b.Versions[j].Version, Purpose: "standard"})
				break
			}
		}
	}
	if len(presets) == 0 {
		return catalog.ModelTemplate{}, fmt.Errorf("quantization %s (kind %s) runs on no backend of the catalog", q.Name, q.Kind)
	}
	arch, contextMax := r.Architecture()
	t := catalog.ModelTemplate{Slug: Slug(r.Repo), Name: r.Repo[strings.Index(r.Repo, "/")+1:], Family: r.extra.ModelType, Task: task,
		ParamsB: tenths(r.Params), ParamsActiveB: tenths(active), Home: r.URL, Aliases: []string{r.Repo},
		DefaultContext: min(contextMax, defaultContext), ContextMax: contextMax, Quantizations: []catalog.Quantization{quant},
		Architecture: arch, License: r.License, Gated: r.Gated, Presets: presets}
	if !r.CreatedAt.IsZero() {
		t.ReleaseDate = r.CreatedAt.UTC().Format(time.DateOnly)
	}
	return t, nil
}
