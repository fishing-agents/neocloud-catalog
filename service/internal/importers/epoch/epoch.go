// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package epoch imports family hardware facts from Epoch AI's CSV merged with
// the curated overlay into the upstream layer. Overlay values are curation;
// Epoch figures never replace curated values, they are listed as conflicts.
package epoch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/fetch"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const (
	CSV = "https://epoch.ai/data/ml_hardware.csv"
	// datasetURL is the page a family cites for Epoch's figures.
	datasetURL = "https://epoch.ai/data/machine-learning-hardware"
	// Epoch's CSV is about 100 kB.
	maxBody = 16 << 20
	// A longer Retry-After fails the run rather than spend jobs.job_timeout.
	maxRetryAfter = 5 * time.Minute
)

var dtypeVocabulary = []string{"fp16", "bf16", "fp8", "fp4", "int8"}

type Importer struct {
	csvURL, overlay, overlayURL string
	getter                      fetch.Getter
}

func New(csvURL, overlay string) (*Importer, error) {
	abs, err := filepath.Abs(overlay)
	if err != nil {
		return nil, err
	}
	return &Importer{csvURL: csvURL, overlay: overlay, overlayURL: (&url.URL{Scheme: "file", Path: abs}).String(),
		getter: fetch.Getter{Client: &http.Client{Timeout: time.Minute}, Backoff: time.Second, Retries: 3, MaxBody: maxBody, MaxRetryAfter: maxRetryAfter}}, nil
}

func (*Importer) Name() string { return "epoch" }

func (i *Importer) Origin() importers.Origin {
	return importers.Origin{Layer: model.LayerUpstream, Source: "epoch", SourceURL: i.csvURL}
}

type sourced[T any] struct {
	Value  T      `json:"value"`
	Source string `json:"source"`
}

// overlayFamily is one curated family; SKU and Note are documentation, not
// imported.
type overlayFamily struct {
	Name               string                      `json:"name"`
	SKU                string                      `json:"sku"`
	Note               string                      `json:"note"`
	EpochName          string                      `json:"epoch_name"`
	Vendor             *sourced[string]            `json:"vendor"`
	Architecture       *sourced[string]            `json:"architecture"`
	ComputeCapability  *sourced[string]            `json:"compute_capability"`
	DTypes             *sourced[[]string]          `json:"dtypes"`
	MemoryGB           *sourced[float64]           `json:"memory_gb"`
	MemoryBandwidthGBs *sourced[float64]           `json:"memory_bandwidth_gbs"`
	TFLOPS             map[string]sourced[float64] `json:"tflops"`
	Interconnect       *sourced[string]            `json:"interconnect"`
	InterconnectGBs    *sourced[float64]           `json:"interconnect_gbs"`
}

func (i *Importer) Run(ctx context.Context, s *store.Store, run uuid.UUID) (importers.Summary, error) {
	overlay, err := readOverlay(i.overlay)
	if err != nil {
		return importers.Summary{}, err
	}
	body, err := i.fetch(ctx)
	if err != nil {
		return importers.Summary{}, err
	}
	rows, err := readEpoch(body)
	if err != nil {
		return importers.Summary{}, fmt.Errorf("%s: %w", i.csvURL, err)
	}
	sum := importers.Summary{Sources: &importers.SourceCounts{}}
	p := store.Patch{CurationURL: i.overlayURL}
	for _, o := range overlay {
		v, err := merge(o, rows)
		if err != nil {
			sum.Errors = append(sum.Errors, o.Name+": "+strings.ReplaceAll(err.Error(), "\n", "; "))
			sum.Sources.Failed++
			continue
		}
		sum.Sources.Read++
		p.Values = append(p.Values, v)
	}
	slices.SortFunc(p.Values, func(a, b store.Values) int { return strings.Compare(a.Key, b.Key) })
	input, err := json.Marshal(struct {
		Values []store.Values
		Errors []string
	}{p.Values, sum.Errors})
	if err != nil {
		return importers.Summary{}, err
	}
	h := sha256.Sum256(input)
	sum.InputVersion = "sha256:" + hex.EncodeToString(h[:])
	if len(p.Values) == 0 {
		return sum, fmt.Errorf("no family could be imported: %s", strings.Join(sum.Errors, "; "))
	}
	sum.Changes, sum.Conflicts, err = s.Patch(ctx, run, p)
	return sum, err
}

func readOverlay(path string) ([]overlayFamily, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var overlay []overlayFamily
	if err := dec.Decode(&overlay); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, o := range overlay {
		if o.Name == "" || seen[o.Name] {
			return nil, fmt.Errorf("%s: family %q: empty or duplicate name", path, o.Name)
		}
		seen[o.Name] = true
	}
	return overlay, nil
}

func (i *Importer) fetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.csvURL, nil)
	if err != nil {
		return nil, err
	}
	body, _, err := i.getter.Get(ctx, req)
	if err != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("GET %s: %w", i.csvURL, err)
	}
	return body, err
}

// epochRow is one CSV row in catalog units; zero means blank, err why it is
// unusable.
type epochRow struct {
	vendor                               string
	memoryGB, bandwidthGBs, intranodeGBs float64
	tflops                               map[string]float64
	err                                  error
}

// readEpoch indexes the CSV by "Hardware name"; Tensor-FP16/BF16 is the dense
// 16-bit rate.
func readEpoch(b []byte) (map[string]epochRow, error) {
	records, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errors.New("empty")
	}
	col := map[string]int{}
	for i, h := range records[0] {
		col[h] = i
	}
	const (
		name      = "Hardware name"
		vendor    = "Manufacturer"
		fp16      = "Tensor-FP16/BF16 performance (FLOP/s)"
		fp8       = "FP8 performance (FLOP/s)"
		fp4       = "FP4 performance (FLOP/s)"
		memory    = "Memory (bytes)"
		bandwidth = "Memory bandwidth (byte/s)"
		intranode = "Intranode bandwidth (byte/s)"
	)
	for _, c := range []string{name, vendor, fp16, fp8, fp4, memory, bandwidth, intranode} {
		if _, ok := col[c]; !ok {
			return nil, fmt.Errorf("no column %q", c)
		}
	}
	rows := map[string]epochRow{}
	for _, rec := range records[1:] {
		var errs []error
		num := func(c string, scale float64) float64 {
			s := strings.TrimSpace(rec[col[c]])
			if s == "" {
				return 0
			}
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				errs = append(errs, fmt.Errorf("Epoch row %q %s: %w", rec[col[name]], c, err))
			}
			return math.Round(v/scale*1000) / 1000
		}
		r := epochRow{
			vendor:       strings.ToLower(strings.TrimSpace(rec[col[vendor]])),
			memoryGB:     num(memory, 1e9),
			bandwidthGBs: num(bandwidth, 1e9),
			intranodeGBs: num(intranode, 1e9),
			tflops:       map[string]float64{},
		}
		for _, t := range []struct {
			c     string
			types []string
		}{{fp16, []string{"fp16", "bf16"}}, {fp8, []string{"fp8"}}, {fp4, []string{"fp4"}}} {
			if v := num(t.c, 1e12); v > 0 {
				for _, d := range t.types {
					r.tflops[d] = v
				}
			}
		}
		key := rec[col[name]]
		if _, dup := rows[key]; dup {
			errs = append(errs, fmt.Errorf("duplicate Epoch row %q", key))
		}
		r.err = errors.Join(errs...)
		rows[key] = r
	}
	return rows, nil
}

func merge(o overlayFamily, rows map[string]epochRow) (store.Values, error) {
	f := catalog.Family{Name: o.Name, TFLOPS: map[string]float64{}}
	if o.EpochName != "" {
		r, ok := rows[o.EpochName]
		switch {
		case !ok:
			return store.Values{}, fmt.Errorf("no Epoch row %q", o.EpochName)
		case r.err != nil:
			return store.Values{}, r.err
		}
		f.Vendor, f.MemoryGB, f.MemoryBandwidthGBs, f.InterconnectGBs = r.vendor, r.memoryGB, r.bandwidthGBs, r.intranodeGBs
		f.Sources = append(f.Sources, datasetURL)
		maps.Copy(f.TFLOPS, r.tflops)
	}
	var curated []string
	err := errors.Join(
		apply("vendor", o.Vendor, &f.Vendor, &f.Sources, &curated),
		apply("architecture", o.Architecture, &f.Architecture, &f.Sources, &curated),
		apply("compute_capability", o.ComputeCapability, &f.ComputeCapability, &f.Sources, &curated),
		apply("dtypes", o.DTypes, &f.DTypes, &f.Sources, &curated),
		apply("memory_gb", o.MemoryGB, &f.MemoryGB, &f.Sources, &curated),
		apply("memory_bandwidth_gbs", o.MemoryBandwidthGBs, &f.MemoryBandwidthGBs, &f.Sources, &curated),
		apply("interconnect", o.Interconnect, &f.Interconnect, &f.Sources, &curated),
		apply("interconnect_gbs", o.InterconnectGBs, &f.InterconnectGBs, &f.Sources, &curated),
	)
	if err != nil {
		return store.Values{}, err
	}
	// Epoch gives figures for non-native dtypes (FP8 on Ampere).
	maps.DeleteFunc(f.TFLOPS, func(d string, _ float64) bool { return !slices.Contains(f.DTypes, d) })
	for _, d := range slices.Sorted(maps.Keys(o.TFLOPS)) {
		if !slices.Contains(f.DTypes, d) {
			return store.Values{}, fmt.Errorf("tflops.%s: not a native dtype", d)
		}
		var v float64
		s := o.TFLOPS[d]
		if err := apply("tflops."+d, &s, &v, &f.Sources, &curated); err != nil {
			return store.Values{}, err
		}
		f.TFLOPS[d] = v
	}
	if len(f.TFLOPS) == 0 {
		f.TFLOPS = nil
	}
	if err := validate(f); err != nil {
		return store.Values{}, err
	}
	if len(curated) > 0 {
		curated = append(curated, "sources")
	}
	slices.Sort(curated)
	return store.Values{Kind: store.KindFamily, Key: f.Name, Curated: curated, Columns: map[string]any{
		"vendor": f.Vendor, "architecture": f.Architecture, "compute_capability": f.ComputeCapability, "dtypes": f.DTypes,
		"memory_gb": f.MemoryGB, "memory_bandwidth_gbs": f.MemoryBandwidthGBs, "tflops": f.TFLOPS,
		"interconnect": f.Interconnect, "interconnect_gbs": f.InterconnectGBs, "sources": f.Sources,
	}}, nil
}

func apply[T any](field string, s *sourced[T], dst *T, sources, curated *[]string) error {
	if s == nil {
		return nil
	}
	if !strings.HasPrefix(s.Source, "https://") {
		return fmt.Errorf("%s: source %q is not an https URL", field, s.Source)
	}
	*dst = s.Value
	if !slices.Contains(*sources, s.Source) {
		*sources = append(*sources, s.Source)
	}
	if column, _, _ := strings.Cut(field, "."); !slices.Contains(*curated, column) {
		*curated = append(*curated, column)
	}
	return nil
}

func validate(f catalog.Family) error {
	var errs []error
	for _, r := range []struct {
		field string
		ok    bool
	}{
		{"vendor", f.Vendor != ""},
		{"architecture", f.Architecture != ""},
		{"compute_capability", f.ComputeCapability != ""},
		{"dtypes", len(f.DTypes) > 0},
		{"memory_gb", f.MemoryGB > 0},
		{"memory_bandwidth_gbs", f.MemoryBandwidthGBs > 0},
	} {
		if !r.ok {
			errs = append(errs, fmt.Errorf("%s: missing (no Epoch figure and no overlay value)", r.field))
		}
	}
	for _, d := range f.DTypes {
		if !slices.Contains(dtypeVocabulary, d) {
			errs = append(errs, fmt.Errorf("dtypes: %q not in %v", d, dtypeVocabulary))
		}
	}
	if (f.Interconnect == "") != (f.InterconnectGBs == 0) {
		errs = append(errs, fmt.Errorf("interconnect %q and interconnect_gbs %v must be set together", f.Interconnect, f.InterconnectGBs))
	}
	return errors.Join(errs...)
}
