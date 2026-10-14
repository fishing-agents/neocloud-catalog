// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Command catalog-export writes the snapshot in every adapter format to
// <out>/<format>/ plus <out>/SHA256SUMS for the release to sign.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
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
	x := adapters.Exporter{Adapter: cfg.adapter, Format: cfg.format, Options: cfg.opts, Extensions: cfg.extensions, Log: log}
	return x.Write(cfg.out, snap)
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
