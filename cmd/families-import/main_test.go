package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	catalog "github.com/fishman/neocloud-catalog"
)

const (
	sampleCSV = "../../testdata/epoch-sample.csv"
	ccURL     = "https://developer.nvidia.com/cuda/gpus"
	vllmURL   = "https://github.com/vllm-project/vllm/blob/v0.31.0/vllm/platforms/cuda.py"
	h100URL   = "https://www.nvidia.com/en-us/data-center/h100/"
	l4URL     = "https://www.nvidia.com/en-us/data-center/l4/"
)

const twoFamilies = `[
  {
    "name": "H100",
    "sku": "NVIDIA H100 SXM5 80 GB",
    "epoch_name": "NVIDIA H100 SXM5 80GB",
    "architecture": {"value": "hopper", "source": "` + h100URL + `"},
    "compute_capability": {"value": "9.0", "source": "` + ccURL + `"},
    "dtypes": {"value": ["fp16", "bf16", "fp8"], "source": "` + vllmURL + `"},
    "interconnect": {"value": "nvlink", "source": "` + h100URL + `"}
  },
  {
    "name": "L4",
    "sku": "NVIDIA L4 24 GB",
    "epoch_name": "NVIDIA L4",
    "architecture": {"value": "ada", "source": "` + l4URL + `"},
    "compute_capability": {"value": "8.9", "source": "` + ccURL + `"},
    "dtypes": {"value": ["fp16", "bf16", "fp8"], "source": "` + vllmURL + `"},
    "interconnect": {"value": "pcie", "source": "` + l4URL + `"},
    "interconnect_gbs": {"value": 64, "source": "` + l4URL + `"},
    "tflops": {"fp8": {"value": 242, "source": "` + l4URL + `"}}
  }
]`

func importOverlay(t *testing.T, overlay string) ([]catalog.Family, error) {
	t.Helper()
	dir := t.TempDir()
	overlayPath := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "families.json")
	if err := run(sampleCSV, overlayPath, out); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var fams []catalog.Family
	if err := json.Unmarshal(b, &fams); err != nil {
		t.Fatal(err)
	}
	return fams, nil
}

func TestImportMergesEpochAndOverlay(t *testing.T) {
	fams, err := importOverlay(t, twoFamilies)
	if err != nil {
		t.Fatal(err)
	}
	if len(fams) != 2 || fams[0].Name != "H100" || fams[1].Name != "L4" {
		t.Fatalf("families = %+v, want H100 then L4", fams)
	}
	h100, l4 := fams[0], fams[1]

	if h100.Vendor != "nvidia" || h100.MemoryGB != 80 || h100.MemoryBandwidthGBs != 3350 {
		t.Errorf("H100 epoch facts = %s %v GB %v GB/s, want nvidia 80 3350", h100.Vendor, h100.MemoryGB, h100.MemoryBandwidthGBs)
	}
	if h100.ComputeCapability != "9.0" || h100.Architecture != "hopper" {
		t.Errorf("H100 overlay facts = %q %q", h100.ComputeCapability, h100.Architecture)
	}
	if !slices.Equal(h100.DTypes, []string{"fp16", "bf16", "fp8"}) {
		t.Errorf("H100 dtypes = %v, want the overlay's", h100.DTypes)
	}
	if h100.Interconnect != "nvlink" || h100.InterconnectGBs != 900 {
		t.Errorf("H100 interconnect = %s %v, want nvlink 900 (epoch intranode)", h100.Interconnect, h100.InterconnectGBs)
	}
	if want := map[string]float64{"fp16": 989.4, "bf16": 989.4, "fp8": 1979}; !maps.Equal(h100.TFLOPS, want) {
		t.Errorf("H100 tflops = %v, want %v", h100.TFLOPS, want)
	}
	for _, url := range []string{epochURL, h100URL, ccURL, vllmURL} {
		if !slices.Contains(h100.Sources, url) {
			t.Errorf("H100 sources %v lack %s", h100.Sources, url)
		}
	}

	// Epoch leaves FP8 blank for L4: dtypes come from the overlay only, and the
	// overlay fills the missing figure.
	if !slices.Equal(l4.DTypes, []string{"fp16", "bf16", "fp8"}) {
		t.Errorf("L4 dtypes = %v", l4.DTypes)
	}
	if want := map[string]float64{"fp16": 121, "bf16": 121, "fp8": 242}; !maps.Equal(l4.TFLOPS, want) {
		t.Errorf("L4 tflops = %v, want %v", l4.TFLOPS, want)
	}
	if l4.Interconnect != "pcie" || l4.InterconnectGBs != 64 || l4.MemoryBandwidthGBs != 300.1 {
		t.Errorf("L4 = %s %v GB/s link, %v GB/s memory", l4.Interconnect, l4.InterconnectGBs, l4.MemoryBandwidthGBs)
	}
}

func TestImportFailsOnMissingRowOrSource(t *testing.T) {
	for name, tc := range map[string]struct{ overlay, want string }{
		"unknown epoch row": {
			`[{"name": "X", "epoch_name": "NVIDIA X9000",
			  "architecture": {"value": "hopper", "source": "` + h100URL + `"},
			  "compute_capability": {"value": "9.0", "source": "` + ccURL + `"},
			  "dtypes": {"value": ["bf16"], "source": "` + vllmURL + `"}}]`,
			"NVIDIA X9000",
		},
		"no epoch row and no manual numbers": {
			`[{"name": "X", "vendor": {"value": "nvidia", "source": "` + h100URL + `"},
			  "architecture": {"value": "hopper", "source": "` + h100URL + `"},
			  "compute_capability": {"value": "9.0", "source": "` + ccURL + `"},
			  "dtypes": {"value": ["bf16"], "source": "` + vllmURL + `"}}]`,
			"memory_gb",
		},
		"value without source": {
			`[{"name": "H100", "epoch_name": "NVIDIA H100 SXM5 80GB",
			  "architecture": {"value": "hopper"},
			  "compute_capability": {"value": "9.0", "source": "` + ccURL + `"},
			  "dtypes": {"value": ["bf16"], "source": "` + vllmURL + `"}}]`,
			"architecture",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := importOverlay(t, tc.overlay)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}
