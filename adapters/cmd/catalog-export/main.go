// Command catalog-export renders the catalog snapshot in the formats of the
// registered platform adapters: <out>/<format>/... per format, and
// <out>/SHA256SUMS over every file for the release to sign. Registered export
// extensions then render their payloads into their adapter's formats. An export
// whose presets are not all exported or skipped with a documented reason fails.
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
	cfg.extensions = adapters.ExportExtensions()
	if err := run(cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "catalog-export:", err)
		os.Exit(1)
	}
}

type config struct {
	adapter, format, snapshot, out string
	opts                           adapters.Options
	extensions                     []adapters.ExportExtension
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
			if err := extend(cfg.extensions, name, format, snap, &e); err != nil {
				return err
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
		return catalog.SnapshotV4()
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

// extend runs the extensions of adapter on its export of format. An extension
// may change the files but not which presets are exported or skipped.
func extend(xs []adapters.ExportExtension, adapter, format string, snap catalog.CatalogSnapshot, e *adapters.Export) error {
	for _, x := range xs {
		if x.Adapter() != adapter {
			continue
		}
		exported, skipped := slices.Clone(e.Exported), slices.Clone(e.Skipped)
		if err := x.Apply(format, snap, e); err != nil {
			return fmt.Errorf("%s %s: extension %s: %w", adapter, format, x.Namespace(), err)
		}
		if !slices.Equal(e.Exported, exported) || !slices.Equal(e.Skipped, skipped) {
			return fmt.Errorf("%s %s: extension %s changed the exported or skipped presets", adapter, format, x.Namespace())
		}
	}
	return nil
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
