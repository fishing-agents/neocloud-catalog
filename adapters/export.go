package adapters

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	catalog "github.com/fishman/neocloud-catalog"
)

// Exporter writes a snapshot in platform formats. Adapter and Format select
// one adapter and one of its formats; empty selects every registered adapter
// and every format. Extensions run on their adapter's exports; Log gets a
// line per format and per skipped preset.
type Exporter struct {
	Adapter, Format string
	Options         Options
	Extensions      []ExportExtension
	Log             io.Writer
}

// Write renders snap in the selected formats to out/<format>/..., refuses an
// export whose presets are not all exported or skipped with a documented
// reason, and writes out/SHA256SUMS over every file for the release to sign.
func (x Exporter) Write(out string, snap catalog.CatalogSnapshot) error {
	names := Names()
	if x.Adapter != "" {
		names = []string{x.Adapter}
	} else if x.Format != "" {
		return errors.New("a format needs its adapter")
	}
	var files []string
	for _, name := range names {
		a, ok := Get(name)
		if !ok {
			return fmt.Errorf("unknown adapter %q (have %s)", name, strings.Join(Names(), ", "))
		}
		formats := a.Formats()
		if x.Format != "" {
			if !slices.Contains(formats, x.Format) {
				return fmt.Errorf("adapter %s has no format %q (have %s)", name, x.Format, strings.Join(formats, ", "))
			}
			formats = []string{x.Format}
		}
		for _, format := range formats {
			e, err := a.Export(snap, format, x.Options)
			if err != nil {
				return fmt.Errorf("%s %s: %w", name, format, err)
			}
			if err := extend(x.Extensions, name, format, snap, &e); err != nil {
				return err
			}
			if err := Coverage(a, snap, format, e); err != nil {
				return err
			}
			for _, art := range e.Artifacts {
				rel := filepath.Join(format, filepath.FromSlash(art.Path))
				if !filepath.IsLocal(rel) {
					return fmt.Errorf("%s %s: artifact path %q leaves the export root", name, format, art.Path)
				}
				if err := write(filepath.Join(out, rel), art.Body); err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(rel))
			}
			fmt.Fprintf(x.Log, "%s %s: %d files, %d presets exported, %d skipped\n", name, format, len(e.Artifacts), len(e.Exported), len(e.Skipped))
			for _, s := range e.Skipped {
				fmt.Fprintf(x.Log, "  skipped %s/%s: %s\n", s.Model, s.Preset, s.Reason)
			}
		}
	}
	return sums(out, files)
}

// extend runs the extensions of adapter on its export of format. An extension
// may change the files but not which presets are exported or skipped.
func extend(xs []ExportExtension, adapter, format string, snap catalog.CatalogSnapshot, e *Export) error {
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
