// Package huggingface imports what the Hugging Face Hub says about the
// effective catalog's huggingface sources at their pinned revisions into the
// upstream layer. Per source it reads the model info and the file tree: the
// tree gives the source's manifest and its quantization's or draft model's
// weight size; for a model's reference source, the first huggingface source
// of a non-gguf quantization, the info and config.json give its parameters,
// active parameters, licence, gating, architecture and maximum context.
// Curated values win (R12) and the run lists where they differ. Objects a
// local override hides are not read: the operator curates those whole.
//
// A source the Hub refuses (a missing revision, a gated repository without
// an accepted token, errors after retries) is listed in the run's errors and
// keeps its rows; the summary counts the sources read in full and those with
// errors. The run fails only when it could read no source, when its write
// fails, or when it is cancelled, which writes nothing. Its input version
// hashes what the Hub returned that the run used, so it changes with the
// imported facts (a token making gated files readable, a licence change)
// and not with download counts.
package huggingface

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/fetch"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Endpoint is the Hugging Face Hub.
const Endpoint = "https://huggingface.co"

// Importer reads the Hub at endpoint, with token for gated repositories.
type Importer struct {
	endpoint, token string
	getter          fetch.Getter
	// interval spaces requests: the Hub allows 500 API requests per five
	// minutes to an anonymous client. A run over the committed catalog makes
	// 33, so it takes well under a minute of jobs.job_timeout.
	interval time.Duration
}

// New returns the importer of the Hub at endpoint; token may be empty.
func New(endpoint, token string) *Importer {
	return &Importer{endpoint: strings.TrimSuffix(endpoint, "/"), token: token, interval: 700 * time.Millisecond,
		getter: fetch.Getter{Client: &http.Client{Timeout: time.Minute}, Backoff: time.Second, Retries: 3, MaxBody: maxBody, MaxRetryAfter: maxRetryAfter}}
}

func (*Importer) Name() string { return "huggingface" }

func (i *Importer) Origin() importers.Origin {
	return importers.Origin{Layer: model.LayerUpstream, Source: "huggingface", SourceURL: i.endpoint}
}

// info is the part of a revision's model info the importer reads.
type info struct {
	SHA      string `json:"sha"`
	Gated    any    `json:"gated"` // false, "auto" or "manual"
	CardData struct {
		License any `json:"license"`
	} `json:"cardData"`
	Safetensors *struct {
		Total int64 `json:"total"`
	} `json:"safetensors"`
}

// entry is a tree entry; LFS holds the content's sha256 of a file stored in
// Git LFS, OID is the Git blob id.
type entry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	OID  string `json:"oid"`
	LFS  *struct {
		OID string `json:"oid"`
	} `json:"lfs"`
}

// config is the part of config.json that sizes the model. MoE lists its keys
// and architectures naming experts or MoE; a dense model has none.
type config struct {
	Layers        int      `json:"num_hidden_layers"`
	Heads         int      `json:"num_attention_heads"`
	KVHeads       int      `json:"num_key_value_heads"`
	HeadDim       int      `json:"head_dim"`
	Hidden        int      `json:"hidden_size"`
	Intermediate  int      `json:"intermediate_size"`
	MaxPositions  int      `json:"max_position_embeddings"`
	Experts       int      `json:"num_local_experts"`
	ExpertsPerTok int      `json:"num_experts_per_tok"`
	Architectures []string `json:"architectures"`
	MoE           []string `json:"-"`
}

func parseConfig(b []byte) (config, error) {
	var c config
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
	}
	if err := json.Unmarshal(b, &keys); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
	}
	moe := func(s string) bool {
		s = strings.ToLower(s)
		return strings.Contains(s, "expert") || strings.Contains(s, "moe")
	}
	for k := range keys {
		if moe(k) {
			c.MoE = append(c.MoE, k)
		}
	}
	for _, a := range c.Architectures {
		if moe(a) {
			c.MoE = append(c.MoE, a)
		}
	}
	slices.Sort(c.MoE)
	if a := c.architecture(); a.Layers == 0 || a.AttentionHeads == 0 || a.HeadDim == 0 || c.MaxPositions == 0 {
		return c, errors.New("config.json lacks the layers, attention heads, head size or maximum position")
	}
	return c, nil
}

func (c config) architecture() catalog.Architecture {
	a := catalog.Architecture{Layers: c.Layers, AttentionHeads: c.Heads, KVHeads: c.KVHeads, HeadDim: c.HeadDim}
	if a.KVHeads == 0 {
		a.KVHeads = c.Heads
	}
	if a.HeadDim == 0 && c.Heads > 0 {
		a.HeadDim = c.Hidden / c.Heads
	}
	return a
}

// mixtral are the expert keys of a Mixtral-style mixture: every layer routes
// each token to num_experts_per_tok of num_local_experts gated MLPs, with no
// shared experts.
var mixtral = []string{"num_experts_per_tok", "num_local_experts"}

// active is the parameters one token runs through: all of a dense model's;
// a Mixtral-style mixture skips the gated MLPs (three hidden x intermediate
// matrices per layer) of the experts it does not route to. Other mixtures
// are not recognised.
func (c config) active(total int64) (int64, error) {
	switch {
	case len(c.MoE) == 0:
		return total, nil
	case slices.Equal(c.MoE, mixtral) && c.Experts > c.ExpertsPerTok && c.ExpertsPerTok > 0:
		return total - int64(c.Experts-c.ExpertsPerTok)*int64(c.Layers)*3*int64(c.Hidden)*int64(c.Intermediate), nil
	}
	return 0, fmt.Errorf("config.json: mixture of experts %v not recognised, active parameters not imported", c.MoE)
}

// job is a source to read: its owner (a quantization or a draft model),
// whether it is a gguf quantization's and whether it is its model's
// reference source.
type job struct {
	owner     store.Manifest
	src       catalog.ModelSource
	gguf, ref bool
}

func (j job) String() string {
	return strings.Trim(j.owner.ModelSlug+"/"+j.owner.Quantization+"/"+j.owner.DraftSlug, "/") + " " + j.src.Repo + "@" + j.src.Revision
}

// plan lists the huggingface sources of the objects in, of models and draft
// models effective in the upstream layer only, in catalog order.
func plan(in store.Inputs, upstream func(kind, key string) bool) []job {
	var jobs []job
	for _, m := range in.Models {
		if !upstream(store.KindModel, m.Slug) {
			continue
		}
		ref := true
		for _, q := range m.Quantizations {
			for _, src := range q.Sources {
				if src.Type != catalog.SourceHuggingFace {
					continue
				}
				gguf := q.Kind == "gguf"
				jobs = append(jobs, job{store.Manifest{ModelSlug: m.Slug, Quantization: q.Name}, src, gguf, ref && !gguf})
				ref = ref && gguf
			}
		}
	}
	for _, d := range in.Drafts {
		if upstream(store.KindDraft, d.Slug) && d.Source.Type == catalog.SourceHuggingFace {
			jobs = append(jobs, job{owner: store.Manifest{DraftSlug: d.Slug}, src: d.Source})
		}
	}
	return jobs
}

// source is what a source's revision says; a part that could not be read is
// zero and its error listed. active is the parameters per token a reference
// source's config gives, 0 when unknown.
type source struct {
	info    *info
	files   []catalog.ModelFile
	weights int64
	config  *config
	active  int64
	errs    []error
}

// record is what a read gave, the input a run's input version hashes.
type record struct {
	Info    *info
	Files   []catalog.ModelFile
	Weights int64
	Config  *config
	MoE     []string
	Errs    []string
}

func (r source) record() record {
	rec := record{Info: r.info, Files: r.files, Weights: r.weights, Config: r.config}
	if r.config != nil {
		rec.MoE = r.config.MoE
	}
	for _, err := range r.errs {
		rec.Errs = append(rec.Errs, err.Error())
	}
	return rec
}

// read reads j's source: its info, then its tree, and config.json for a
// reference source.
func (c *client) read(ctx context.Context, j job) source {
	var r source
	api := c.endpoint + "/api/models/" + j.src.Repo
	var inf info
	if err := c.getJSON(ctx, api+"/revision/"+j.src.Revision, &inf); err != nil {
		r.errs = append(r.errs, err)
		return r
	}
	if inf.SHA != j.src.Revision {
		r.errs = append(r.errs, fmt.Errorf("revision %s is commit %s: pin the commit", j.src.Revision, inf.SHA))
		return r
	}
	r.info = &inf
	tree, err := c.tree(ctx, api+"/tree/"+j.src.Revision+"?recursive=1")
	if err == nil {
		r.files, r.weights, err = manifest(tree, j.gguf, j.src.Files)
		if errors.Is(err, errDigestHidden) {
			err = c.gated(err)
		}
	}
	if err != nil {
		r.errs = append(r.errs, err)
	}
	if !j.ref {
		return r
	}
	body, _, err := c.get(ctx, c.endpoint+"/"+j.src.Repo+"/resolve/"+j.src.Revision+"/config.json")
	var cfg config
	if err == nil {
		cfg, err = parseConfig(body)
	}
	if err != nil {
		r.errs = append(r.errs, err)
		return r
	}
	r.config = &cfg
	if inf.Safetensors != nil && inf.Safetensors.Total > 0 {
		if r.active, err = cfg.active(inf.Safetensors.Total); err != nil {
			r.errs = append(r.errs, err)
		}
	}
	return r
}

var errDigestHidden = errors.New("LFS digest hidden")

// weightFormats are the weight file formats, in the order vLLM looks for
// them: it loads only the first one a revision has.
var weightFormats = []string{".safetensors", ".bin", ".pt", ".pth"}

// manifest is the files of tree a server fetches: all but empty files and the
// weights in formats other than the first present (Meta's original/*.pth,
// Mistral's consolidated *.pt). A gguf quantization's manifest is its curated
// selection as the tree has it, since the repository holds every
// quantization. weights is the size of the manifest's weight files; it is
// known even when a digest is not (a gated repository read without a token),
// the manifest then is not.
func manifest(tree []entry, gguf bool, selection []catalog.ModelFile) ([]catalog.ModelFile, int64, error) {
	var keep []entry
	var weights int64
	if gguf {
		if len(selection) == 0 {
			return nil, 0, errors.New("gguf source without curated files: the repository holds every quantization")
		}
		for _, f := range selection {
			i := slices.IndexFunc(tree, func(e entry) bool { return e.Type == "file" && e.Path == f.Path })
			if i < 0 {
				return nil, 0, fmt.Errorf("file %s: not in the revision", f.Path)
			}
			keep = append(keep, tree[i])
			weights += tree[i].Size
		}
	} else {
		files := slices.DeleteFunc(slices.Clone(tree), func(e entry) bool { return e.Type != "file" || e.Size == 0 })
		format := ""
		for _, ext := range weightFormats {
			if slices.ContainsFunc(files, func(e entry) bool { return path.Ext(e.Path) == ext }) {
				format = ext
				break
			}
		}
		for _, e := range files {
			switch ext := path.Ext(e.Path); {
			case format != "" && ext == format:
				weights += e.Size
			case slices.Contains(weightFormats, ext):
				continue
			}
			keep = append(keep, e)
		}
	}
	files := make([]catalog.ModelFile, len(keep))
	for i, e := range keep {
		alg, sum, n := "git-oid", e.OID, 40
		if e.LFS != nil {
			alg, sum, n = "sha256", e.LFS.OID, 64
		}
		if strings.Trim(sum, "*") == "" {
			return nil, weights, fmt.Errorf("file %s: %w", e.Path, errDigestHidden)
		}
		if len(sum) != n || strings.Trim(sum, "0123456789abcdef") != "" || e.Size <= 0 {
			return nil, weights, fmt.Errorf("file %s: size %d digest %s:%s unusable", e.Path, e.Size, alg, sum)
		}
		files[i] = catalog.ModelFile{Path: e.Path, Size: e.Size, Digest: alg + ":" + sum}
	}
	return files, weights, nil
}

// tenths rounds n to tenths of a billion, the catalog's unit of parameters
// (B) and weights (GB).
func tenths(n int64) float64 { return math.Round(float64(n)/1e8) / 10 }

// modelValues are the model columns a reference source's read gives.
func modelValues(r source) map[string]any {
	cols := map[string]any{"gated": r.info.Gated != nil && r.info.Gated != false}
	if license, ok := r.info.CardData.License.(string); ok && license != "" {
		cols["license"] = license
	}
	var params int64
	if r.info.Safetensors != nil && r.info.Safetensors.Total > 0 {
		params = r.info.Safetensors.Total
		cols["params_b"] = tenths(params)
	}
	if r.config != nil {
		cols["architecture"] = r.config.architecture()
		cols["context_max"] = r.config.MaxPositions
	}
	if r.active > 0 {
		cols["params_active_b"] = tenths(r.active)
	}
	return cols
}

func (i *Importer) Run(ctx context.Context, s *store.Store, run uuid.UUID) (importers.Summary, error) {
	in, prov, err := s.EffectiveProvenance(ctx)
	if err != nil {
		return importers.Summary{}, err
	}
	jobs := plan(in, func(kind, key string) bool {
		es := prov[kind][key]
		e := slices.IndexFunc(es, func(e store.ProvenanceEntry) bool { return e.Effective })
		return e >= 0 && es[e].Layer == model.LayerUpstream
	})
	c := &client{Importer: i}
	h := sha256.New()
	enc := json.NewEncoder(h)
	sum := importers.Summary{Sources: &importers.SourceCounts{}}
	var p store.Patch
	weighed := map[string]bool{}
	readable := 0
	for _, j := range jobs {
		r := c.read(ctx, j)
		if err := ctx.Err(); err != nil {
			return importers.Summary{}, err
		}
		if err := enc.Encode(struct {
			Job string
			record
		}{j.String(), r.record()}); err != nil {
			return importers.Summary{}, err
		}
		for _, err := range r.errs {
			sum.Errors = append(sum.Errors, fmt.Sprintf("%s: %v", j, err))
		}
		if len(r.errs) > 0 {
			sum.Sources.Failed++
		} else {
			sum.Sources.Read++
		}
		if r.info == nil {
			continue
		}
		readable++
		if j.ref {
			p.Values = append(p.Values, store.Values{Kind: store.KindModel, Key: j.owner.ModelSlug, Columns: modelValues(r)})
		}
		kind, key := store.KindQuantization, j.owner.ModelSlug+"/"+j.owner.Quantization
		if j.owner.DraftSlug != "" {
			kind, key = store.KindDraft, j.owner.DraftSlug
		}
		// A quantization weighs what its first readable source holds.
		if r.weights > 0 && !weighed[key] {
			weighed[key] = true
			p.Values = append(p.Values, store.Values{Kind: kind, Key: key, Columns: map[string]any{"weights_gb": tenths(r.weights)}})
		}
		if r.files != nil {
			m := j.owner
			m.Type, m.Repo, m.Revision, m.Files = j.src.Type, j.src.Repo, j.src.Revision, r.files
			p.Manifests = append(p.Manifests, m)
		}
	}
	sum.InputVersion = "sha256:" + hex.EncodeToString(h.Sum(nil))
	if readable == 0 && len(jobs) > 0 {
		return sum, fmt.Errorf("no source could be read: %s", strings.Join(sum.Errors, "; "))
	}
	sum.Changes, sum.Conflicts, err = s.Patch(ctx, run, p)
	return sum, err
}
