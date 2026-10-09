// Package kaito cross-checks catalog architectures and max context against KAITO's
// model_catalog.yaml at a pinned release. It never writes: KAITO derives from the
// same config.json, so a mismatch is listed for an operator to review.
package kaito

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/fetch"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const (
	// Revision is KAITO v0.12.0's commit.
	Revision = "3be724e6e2d8b351e4db011935b7c31b02205b82"
	Catalog  = "https://raw.githubusercontent.com/kaito-project/kaito/" + Revision + "/presets/workspace/models/model_catalog.yaml"
	// KAITO's is about 24 kB.
	maxBody       = 4 << 20
	maxRetryAfter = 5 * time.Minute
)

type Importer struct {
	url    string
	getter fetch.Getter
}

func New(url string) *Importer {
	return &Importer{url: url,
		getter: fetch.Getter{Client: &http.Client{Timeout: time.Minute}, Backoff: time.Second, Retries: 3, MaxBody: maxBody, MaxRetryAfter: maxRetryAfter}}
}

func (*Importer) Name() string { return "kaito" }

func (i *Importer) Origin() importers.Origin {
	return importers.Origin{Layer: model.LayerUpstream, Source: "kaito", SourceURL: i.url}
}

// entry is a KAITO catalog entry: a value config.json lacks is 0, and HeadDim is
// 0 unless it differs from hidden size / attention heads.
type entry struct {
	Name       string `yaml:"name" json:"Name"`
	TokenLimit int    `yaml:"modelTokenLimit" json:"TokenLimit"`
	Hidden     int    `yaml:"hiddenSize" json:"Hidden"`
	Layers     int    `yaml:"numHiddenLayers" json:"Layers"`
	Heads      int    `yaml:"numAttentionHeads" json:"Heads"`
	KVHeads    int    `yaml:"numKeyValueHeads" json:"KVHeads"`
	HeadDim    int    `yaml:"headDim" json:"HeadDim"`
}

// architecture treats no KV head count as multi-head attention; ok is false if
// a value is missing.
func (e entry) architecture() (a catalog.Architecture, ok bool) {
	a = catalog.Architecture{Layers: e.Layers, AttentionHeads: e.Heads, KVHeads: e.KVHeads, HeadDim: e.HeadDim}
	if a.KVHeads == 0 {
		a.KVHeads = a.AttentionHeads
	}
	if a.HeadDim == 0 && e.Heads > 0 {
		a.HeadDim = e.Hidden / e.Heads
	}
	return a, a.Layers > 0 && a.AttentionHeads > 0 && a.HeadDim > 0
}

func (i *Importer) Run(ctx context.Context, s *store.Store, run uuid.UUID) (importers.Summary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.url, nil)
	if err != nil {
		return importers.Summary{}, err
	}
	body, _, err := i.getter.Get(ctx, req)
	if err != nil {
		if ctx.Err() == nil {
			err = fmt.Errorf("GET %s: %w", i.url, err)
		}
		return importers.Summary{}, err
	}
	var doc struct {
		Models []entry `yaml:"models"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return importers.Summary{}, fmt.Errorf("%s: %w", i.url, err)
	}
	if len(doc.Models) == 0 {
		return importers.Summary{}, fmt.Errorf("%s lists no model", i.url)
	}
	entries := map[string]entry{}
	for _, e := range doc.Models {
		entries[strings.ToLower(e.Name)] = e
	}
	in, err := s.Effective(ctx)
	if err != nil {
		return importers.Summary{}, err
	}
	sum := importers.Summary{Changes: store.Changes{}, Sources: &importers.SourceCounts{}}
	var used []entry
	for _, m := range in.Models {
		e, ok := match(m, entries)
		if !ok {
			continue
		}
		used = append(used, e)
		differs := func(field string, curated, imported any) {
			sum.Conflicts = append(sum.Conflicts, store.Conflict{Kind: store.KindModel, Key: m.Slug, Field: field, Curated: curated, Imported: imported})
		}
		var lacks []string
		if a, ok := e.architecture(); !ok {
			lacks = append(lacks, "the layers, attention heads or hidden size")
		} else if a != m.Architecture {
			differs("architecture", m.Architecture, a)
		}
		if e.TokenLimit == 0 {
			lacks = append(lacks, "the token limit")
		} else if e.TokenLimit != m.ContextMax {
			differs("context_max", m.ContextMax, e.TokenLimit)
		}
		if len(lacks) > 0 {
			sum.Errors = append(sum.Errors, fmt.Sprintf("%s: KAITO's %s lacks %s", m.Slug, e.Name, strings.Join(lacks, " and ")))
			sum.Sources.Failed++
		} else {
			sum.Sources.Read++
		}
	}
	slices.SortFunc(used, func(a, b entry) int { return cmp.Compare(a.Name, b.Name) })
	input, err := json.Marshal(slices.Compact(used))
	if err != nil {
		return importers.Summary{}, err
	}
	h := sha256.Sum256(input)
	sum.InputVersion = "sha256:" + hex.EncodeToString(h[:])
	return sum, nil
}

// match matches repository ids case-insensitively, as the Hub does.
func match(m catalog.ModelTemplate, entries map[string]entry) (entry, bool) {
	for _, q := range m.Quantizations {
		for _, src := range q.Sources {
			if e, ok := entries[strings.ToLower(src.Repo)]; ok && src.Type == catalog.SourceHuggingFace {
				return e, true
			}
		}
	}
	return entry{}, false
}
