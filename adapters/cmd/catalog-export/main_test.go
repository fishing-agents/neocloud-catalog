package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The release publishes this command's output and signs SHA256SUMS: the output
// must equal each adapter's golden files, and the sums must cover exactly the
// files written, with their hashes.
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
