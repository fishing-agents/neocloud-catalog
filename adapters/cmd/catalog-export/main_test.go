// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

// Output equals the goldens and SHA256SUMS covers exactly the files written.
func TestExportMatchesGoldensAndSums(t *testing.T) {
	out := t.TempDir()
	if err := run(config{out: out}, io.Discard); err != nil {
		t.Fatal(err)
	}
	goldens := map[string]string{
		"gpustack":             "../../gpustack/testdata/gpustack",
		"airunway-recipes":     "../../airunway/testdata/airunway-recipes",
		"airunway-deployments": "../../airunway/testdata/airunway-deployments",
	}
	written := map[string]bool{}
	for format, golden := range goldens {
		err := filepath.WalkDir(golden, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(golden, p)
			want, _ := os.ReadFile(p)
			got, err := os.ReadFile(filepath.Join(out, format, rel))
			if err != nil || string(got) != string(want) {
				t.Errorf("%s/%s differs from the golden (err %v)", format, rel, err)
			}
			written[filepath.ToSlash(filepath.Join(format, rel))] = true
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	f, err := os.Open(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	listed := 0
	for sc := bufio.NewScanner(f); sc.Scan(); {
		sum, path, ok := strings.Cut(sc.Text(), "  ")
		if !ok || !written[path] {
			t.Errorf("SHA256SUMS line %q names no exported file", sc.Text())
			continue
		}
		body, _ := os.ReadFile(filepath.Join(out, filepath.FromSlash(path)))
		if h := sha256.Sum256(body); hex.EncodeToString(h[:]) != sum {
			t.Errorf("SHA256SUMS hash of %s is wrong", path)
		}
		listed++
	}
	if listed != len(written) {
		t.Errorf("SHA256SUMS lists %d files, %d were exported", listed, len(written))
	}
}

func TestRunRefusesUnknownSelection(t *testing.T) {
	for name, cfg := range map[string]config{
		"unknown adapter":        {adapter: "tgi"},
		"unknown format":         {adapter: "gpustack", format: "helm"},
		"format without adapter": {format: "gpustack"},
	} {
		cfg.out = t.TempDir()
		if err := run(cfg, io.Discard); err == nil {
			t.Errorf("%s: run accepted it", name)
		}
	}
}

// annotate stands in for an enterprise extension rendering its payload.
type annotate struct{}

func (annotate) Namespace() string { return "example.com/test" }
func (annotate) Adapter() string   { return "airunway" }
func (annotate) Apply(format string, _ catalog.CatalogSnapshot, e *adapters.Export) error {
	for i, art := range e.Artifacts {
		if format != "airunway-deployments" || art.Path != "deployments/llama-3.1-8b/vllm-bf16.yaml" {
			continue
		}
		var md map[string]any
		if err := json.Unmarshal(art.Body, &md); err != nil {
			return err
		}
		md["metadata"].(map[string]any)["annotations"].(map[string]any)["example.com/test"] = "on"
		body, err := json.Marshal(md)
		if err != nil {
			return err
		}
		e.Artifacts[i].Body = body
	}
	return nil
}

// unexport drops a preset from the export it extends.
type unexport struct{}

func (unexport) Namespace() string { return "example.com/unexport" }
func (unexport) Adapter() string   { return "airunway" }
func (unexport) Apply(_ string, _ catalog.CatalogSnapshot, e *adapters.Export) error {
	e.Exported = e.Exported[1:]
	return nil
}

// Extensions may change files, which the sums cover, but not which presets are exported.
func TestExportExtensions(t *testing.T) {
	out := t.TempDir()
	cfg := config{adapter: "airunway", format: "airunway-deployments", out: out, extensions: []adapters.ExportExtension{annotate{}}}
	if err := run(cfg, io.Discard); err != nil {
		t.Fatal(err)
	}
	path := "airunway-deployments/deployments/llama-3.1-8b/vllm-bf16.yaml"
	body, err := os.ReadFile(filepath.Join(out, path))
	if err != nil || !strings.Contains(string(body), `"example.com/test":"on"`) {
		t.Errorf("%s = %s (err %v), want the extension's annotation", path, body, err)
	}
	sums, _ := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	h := sha256.Sum256(body)
	if !strings.Contains(string(sums), hex.EncodeToString(h[:])+"  "+path) {
		t.Errorf("SHA256SUMS does not cover the extended %s", path)
	}

	cfg.extensions = []adapters.ExportExtension{unexport{}}
	cfg.out = t.TempDir()
	if err := run(cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "example.com/unexport") {
		t.Errorf("an extension changing the exported presets: err = %v", err)
	}

	// An extension of another adapter does not run.
	cfg = config{adapter: "gpustack", out: t.TempDir(), extensions: []adapters.ExportExtension{unexport{}}}
	if err := run(cfg, io.Discard); err != nil {
		t.Errorf("an airunway extension ran on gpustack: %v", err)
	}
}
