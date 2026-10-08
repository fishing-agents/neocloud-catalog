// Command catalog-export renders the catalog snapshot in the formats of the
// registered platform adapters: <out>/<format>/... per format, and
// <out>/SHA256SUMS over every file for the release to sign. An export whose
// presets are not all exported or skipped with a documented reason fails.
package main

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
	_ "github.com/fishman/neocloud-catalog/adapters/airunway"
	_ "github.com/fishman/neocloud-catalog/adapters/gpustack"
)

func main() {
	var cfg config
	flag.StringVar(&cfg.adapter, "adapter", "", "adapter name (default: all registered: "+strings.Join(adapters.Names(), ", ")+")")
	flag.StringVar(&cfg.format, "format", "", "one format of the adapter (default: all its formats)")
	flag.StringVar(&cfg.snapshot, "snapshot", "", "snapshot JSON file (default: the snapshot embedded in this build)")
	flag.StringVar(&cfg.out, "out", "dist", "output directory")
	flag.StringVar(&cfg.opts.ReferenceFamily, "reference-family", adapters.DefaultReferenceFamily, "device family whose fit sets the GPU count on platforms without a fit matrix")
	flag.Parse()
	if err := run(cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "catalog-export:", err)
		os.Exit(1)
	}
}

type config struct {
	adapter, format, snapshot, out string
	opts                           adapters.Options
}

func run(cfg config, log io.Writer) error {
	snap, err := load(cfg.snapshot)
	if err != nil {
		return err
	}
	names := adapters.Names()
	if cfg.adapter != "" {
		names = []string{cfg.adapter}
	} else if cfg.format != "" {
		return errors.New("-format needs -adapter")
	}
	var files []string
	for _, name := range names {
		a, ok := adapters.Get(name)
		if !ok {
			return fmt.Errorf("unknown adapter %q (have %s)", name, strings.Join(adapters.Names(), ", "))
		}
		formats := a.Formats()
		if cfg.format != "" {
			if !slices.Contains(formats, cfg.format) {
				return fmt.Errorf("adapter %s has no format %q (have %s)", name, cfg.format, strings.Join(formats, ", "))
			}
			formats = []string{cfg.format}
		}
		for _, format := range formats {
			e, err := a.Export(snap, format, cfg.opts)
			if err != nil {
				return fmt.Errorf("%s %s: %w", name, format, err)
			}
			if err := adapters.Coverage(a, snap, format, e); err != nil {
				return err
			}
			for _, art := range e.Artifacts {
				rel := filepath.Join(format, filepath.FromSlash(art.Path))
				if !filepath.IsLocal(rel) {
					return fmt.Errorf("%s %s: artifact path %q leaves the export root", name, format, art.Path)
				}
				if err := write(filepath.Join(cfg.out, rel), art.Body); err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(rel))
			}
			fmt.Fprintf(log, "%s %s: %d files, %d presets exported, %d skipped\n", name, format, len(e.Artifacts), len(e.Exported), len(e.Skipped))
			for _, s := range e.Skipped {
				fmt.Fprintf(log, "  skipped %s/%s: %s\n", s.Model, s.Preset, s.Reason)
			}
		}
	}
	return sums(cfg.out, files)
}

func load(path string) (catalog.CatalogSnapshot, error) {
	if path == "" {
		return catalog.SnapshotV3()
	}
	var snap catalog.CatalogSnapshot
	b, err := os.ReadFile(path)
	if err != nil {
		return snap, err
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return snap, fmt.Errorf("%s: %w", path, err)
	}
	return snap, snap.Validate()
}

func write(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// sums writes SHA256SUMS in sha256sum's format, sorted by path.
func sums(out string, files []string) error {
	slices.SortFunc(files, cmp.Compare)
	var b strings.Builder
	for _, f := range files {
		body, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(f)))
		if err != nil {
			return err
		}
		h := sha256.Sum256(body)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h[:]), f)
	}
	return write(filepath.Join(out, "SHA256SUMS"), []byte(b.String()))
}
