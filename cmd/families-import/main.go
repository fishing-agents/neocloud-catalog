// Command families-import writes data/families.json from Epoch AI's ML
// hardware data and the hand-curated data/families.overlay.json. Epoch supplies
// vendor, memory, bandwidth, dense tensor TFLOPS and NVLink-class bandwidth;
// the overlay maps each family to its Epoch row and supplies architecture,
// compute capability, native dtypes, interconnect and any Epoch gap or
// correction, every value with its source URL.
package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	catalog "github.com/fishman/neocloud-catalog"
)

const (
	epochCSV = "https://epoch.ai/data/ml_hardware.csv"
	epochURL = "https://epoch.ai/data/machine-learning-hardware"
)

var dtypeVocabulary = []string{"fp16", "bf16", "fp8", "fp4", "int8"}

type sourced[T any] struct {
	Value  T      `json:"value"`
	Source string `json:"source"`
}

// overlayFamily is one curated family. SKU (the product the name stands for)
// and Note (reasoning behind a value) document the curation and are not
// exported.
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

func main() {
	csvPath := flag.String("csv", "", "Epoch ML hardware CSV (default: download "+epochCSV+")")
	overlay := flag.String("overlay", "data/families.overlay.json", "curated overlay")
	out := flag.String("out", "data/families.json", "output")
	flag.Parse()
	if err := importFamilies(*csvPath, *overlay, *out); err != nil {
		fmt.Fprintln(os.Stderr, "families-import:", err)
		os.Exit(1)
	}
}

func importFamilies(csvPath, overlay, out string) error {
	if csvPath == "" {
		tmp, err := download(epochCSV)
		if err != nil {
			return err
		}
		defer os.Remove(tmp)
		csvPath = tmp
	}
	return run(csvPath, overlay, out)
}

func download(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.CreateTemp("", "ml_hardware-*.csv")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func run(csvPath, overlayPath, out string) error {
	rows, err := readEpoch(csvPath)
	if err != nil {
		return err
	}
	of, err := os.Open(overlayPath)
	if err != nil {
		return err
	}
	defer of.Close()
	dec := json.NewDecoder(of)
	dec.DisallowUnknownFields()
	var overlay []overlayFamily
	if err := dec.Decode(&overlay); err != nil {
		return fmt.Errorf("%s: %w", overlayPath, err)
	}
	families := make([]catalog.Family, 0, len(overlay))
	for _, o := range overlay {
		f, err := merge(o, rows)
		if err != nil {
			return fmt.Errorf("%s: %w", o.Name, err)
		}
		families = append(families, f)
	}
	slices.SortFunc(families, func(a, b catalog.Family) int { return strings.Compare(a.Name, b.Name) })
	for i := 1; i < len(families); i++ {
		if families[i].Name == families[i-1].Name {
			return fmt.Errorf("%s: duplicate family", families[i].Name)
		}
	}
	enc, err := json.MarshalIndent(families, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(enc, '\n'), 0o644)
}

// epochRow holds the figures taken from one CSV row, already in catalog units;
// zero means the CSV cell was blank.
type epochRow struct {
	vendor                               string
	memoryGB, bandwidthGBs, intranodeGBs float64
	tflops                               map[string]float64
}

// readEpoch indexes the CSV by "Hardware name". Tensor-FP16/BF16 is the dense
// rate of both 16-bit types.
func readEpoch(path string) (map[string]epochRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("%s: empty", path)
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
			return nil, fmt.Errorf("%s: no column %q", path, c)
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
				errs = append(errs, fmt.Errorf("%q %s: %w", rec[col[name]], c, err))
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
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		key := rec[col[name]]
		if _, dup := rows[key]; dup {
			return nil, fmt.Errorf("%s: duplicate row %q", path, key)
		}
		rows[key] = r
	}
	return rows, nil
}

func merge(o overlayFamily, rows map[string]epochRow) (catalog.Family, error) {
	f := catalog.Family{Name: o.Name, TFLOPS: map[string]float64{}}
	if o.EpochName != "" {
		r, ok := rows[o.EpochName]
		if !ok {
			return f, fmt.Errorf("no Epoch row %q", o.EpochName)
		}
		f.Vendor, f.MemoryGB, f.MemoryBandwidthGBs, f.InterconnectGBs = r.vendor, r.memoryGB, r.bandwidthGBs, r.intranodeGBs
		f.Sources = append(f.Sources, epochURL)
		for d, v := range r.tflops {
			f.TFLOPS[d] = v
		}
	}
	err := errors.Join(
		apply("vendor", o.Vendor, &f.Vendor, &f.Sources),
		apply("architecture", o.Architecture, &f.Architecture, &f.Sources),
		apply("compute_capability", o.ComputeCapability, &f.ComputeCapability, &f.Sources),
		apply("dtypes", o.DTypes, &f.DTypes, &f.Sources),
		apply("memory_gb", o.MemoryGB, &f.MemoryGB, &f.Sources),
		apply("memory_bandwidth_gbs", o.MemoryBandwidthGBs, &f.MemoryBandwidthGBs, &f.Sources),
		apply("interconnect", o.Interconnect, &f.Interconnect, &f.Sources),
		apply("interconnect_gbs", o.InterconnectGBs, &f.InterconnectGBs, &f.Sources),
	)
	if err != nil {
		return f, err
	}
	// Epoch figures exist for dtypes a family may lack natively (FP8 on
	// Ampere); only native dtypes keep a TFLOPS entry.
	maps.DeleteFunc(f.TFLOPS, func(d string, _ float64) bool { return !slices.Contains(f.DTypes, d) })
	for _, d := range slices.Sorted(maps.Keys(o.TFLOPS)) {
		if !slices.Contains(f.DTypes, d) {
			return f, fmt.Errorf("tflops.%s: not a native dtype", d)
		}
		var v float64
		s := o.TFLOPS[d]
		if err := apply("tflops."+d, &s, &v, &f.Sources); err != nil {
			return f, err
		}
		f.TFLOPS[d] = v
	}
	if len(f.TFLOPS) == 0 {
		f.TFLOPS = nil
	}
	return f, validate(f)
}

func apply[T any](field string, s *sourced[T], dst *T, sources *[]string) error {
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
