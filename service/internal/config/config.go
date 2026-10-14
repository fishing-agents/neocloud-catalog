// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package config loads embedded TOML defaults, then an optional directory of
// *.toml in lexical order, then NEOCLOUD_CATALOG_<KEY> env overrides.
package config

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

//go:embed defaults.toml
var defaultsFS embed.FS

const envPrefix = "NEOCLOUD_CATALOG_"

type Config struct {
	Server      ServerConfig      `koanf:"server"`
	Database    DatabaseConfig    `koanf:"database"`
	Telemetry   TelemetryConfig   `koanf:"telemetry"`
	Bootstrap   BootstrapConfig   `koanf:"bootstrap"`
	HuggingFace HuggingFaceConfig `koanf:"huggingface"`
	AzurePrices AzurePricesConfig `koanf:"azure_prices"`
	Auth        AuthConfig        `koanf:"auth"`
	Jobs        JobsConfig        `koanf:"jobs"`
	Release     ReleaseConfig     `koanf:"release"`
	Upstream    UpstreamConfig    `koanf:"upstream"`
}

type ServerConfig struct {
	Addr         string        `koanf:"addr"`
	ReadTimeout  time.Duration `koanf:"read_timeout"`
	WriteTimeout time.Duration `koanf:"write_timeout"`
}

type DatabaseConfig struct {
	DSN      string `koanf:"dsn"`
	MaxConns int    `koanf:"max_conns"`
}

type TelemetryConfig struct {
	MetricsAddr string `koanf:"metrics_addr"`
}

// BootstrapConfig locates the core repository's data/; relative to the working directory.
type BootstrapConfig struct {
	DataDir string `koanf:"data_dir"`
}

// HuggingFaceConfig: TokenFile is read once at startup; rotation needs a restart.
type HuggingFaceConfig struct {
	TokenFile string `koanf:"token_file"`
}

// AzurePricesConfig: Regions are ARM region names.
type AzurePricesConfig struct {
	Regions []string `koanf:"regions"`
}

// armRegion keeps the importer's OData filter well formed.
var armRegion = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// AuthConfig: an issuer_url enables OIDC JWTs whose aud contains audience.
type AuthConfig struct {
	IssuerURL string `koanf:"issuer_url"`
	Audience  string `koanf:"audience"`
}

// JobsConfig: a run still running after RescueAfter (> JobTimeout) was left by
// an exited worker and is failed.
type JobsConfig struct {
	JobTimeout  time.Duration `koanf:"job_timeout"`
	RescueAfter time.Duration `koanf:"rescue_after"`
}

// ReleaseConfig is where the API publishes releases; empty Ref and Key disable
// publishing. PublishTimeout outlasts server.write_timeout.
type ReleaseConfig struct {
	Ref            string        `koanf:"ref"`
	Key            string        `koanf:"key"`
	OverlayRef     string        `koanf:"overlay_ref"`
	PublishTimeout time.Duration `koanf:"publish_timeout"`
	PlainHTTP      bool          `koanf:"plain_http"`
}

// UpstreamConfig is the signed upstream release (and overlay) an operator
// deployment imports. Empty Ref: no upstream importer, no other key may be set.
type UpstreamConfig struct {
	Ref                   string `koanf:"ref"`
	IdentityIssuer        string `koanf:"identity_issuer"`
	Identity              string `koanf:"identity"`
	PublicKey             string `koanf:"public_key"`
	OverlayRef            string `koanf:"overlay_ref"`
	OverlayIdentityIssuer string `koanf:"overlay_identity_issuer"`
	OverlayIdentity       string `koanf:"overlay_identity"`
	OverlayPublicKey      string `koanf:"overlay_public_key"`
	TrustedRoot           string `koanf:"trusted_root"`
	PlainHTTP             bool   `koanf:"plain_http"`
}

// applyEnv matches NEOCLOUD_CATALOG_<NAME> to the known key equal to NAME with
// "." and "_" ignored (case-insensitive), so keys containing "_" are reachable.
// Unknown or ambiguous names are ignored.
func applyEnv(k *koanf.Koanf) error {
	strip := func(s string) string {
		return strings.ToLower(strings.NewReplacer(".", "", "_", "").Replace(s))
	}
	keys := map[string]string{}
	ambiguous := map[string]bool{}
	for _, key := range k.Keys() {
		sk := strip(key)
		if _, dup := keys[sk]; dup {
			ambiguous[sk] = true
		}
		keys[sk] = key
	}
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		n, ok := strings.CutPrefix(name, envPrefix)
		if !ok {
			continue
		}
		sk := strip(n)
		key, ok := keys[sk]
		if !ok || ambiguous[sk] {
			continue
		}
		if err := k.Set(key, val); err != nil {
			return fmt.Errorf("env %s: %w", name, err)
		}
	}
	return nil
}

// Load returns the merged, validated Config; an empty overlayDir is skipped.
func Load(overlayDir string) (*Config, error) {
	k := koanf.New(".")

	def, err := defaultsFS.ReadFile("defaults.toml")
	if err != nil {
		return nil, fmt.Errorf("read embedded defaults: %w", err)
	}
	if err := k.Load(rawbytes.Provider(def), toml.Parser()); err != nil {
		return nil, fmt.Errorf("load embedded defaults: %w", err)
	}

	if overlayDir != "" {
		files, _ := filepath.Glob(filepath.Join(overlayDir, "*.toml"))
		sort.Strings(files)
		for _, f := range files {
			if err := k.Load(file.Provider(f), toml.Parser()); err != nil {
				return nil, fmt.Errorf("load overlay %s: %w", f, err)
			}
		}
	}

	if err := applyEnv(k); err != nil {
		return nil, err
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Database.DSN == "" {
		return errors.New("database.dsn is required")
	}
	if c.Database.MaxConns < 0 {
		return fmt.Errorf("database.max_conns must not be negative, got %d", c.Database.MaxConns)
	}
	if c.Server.Addr == "" {
		return errors.New("server.addr is required")
	}
	if c.Telemetry.MetricsAddr == "" {
		return errors.New("telemetry.metrics_addr is required")
	}
	if c.Telemetry.MetricsAddr == c.Server.Addr {
		return fmt.Errorf("telemetry.metrics_addr must differ from server.addr (%s)", c.Server.Addr)
	}
	for _, d := range []struct {
		key string
		d   time.Duration
	}{{"server.read_timeout", c.Server.ReadTimeout}, {"server.write_timeout", c.Server.WriteTimeout}, {"jobs.job_timeout", c.Jobs.JobTimeout},
		{"release.publish_timeout", c.Release.PublishTimeout}} {
		if d.d <= 0 {
			return fmt.Errorf("%s must be a positive duration, got %s", d.key, d.d)
		}
	}
	if c.Jobs.RescueAfter <= c.Jobs.JobTimeout {
		return fmt.Errorf("jobs.rescue_after (%s) must exceed jobs.job_timeout (%s)", c.Jobs.RescueAfter, c.Jobs.JobTimeout)
	}
	if c.Auth.IssuerURL != "" && c.Auth.Audience == "" {
		return errors.New("auth.audience is required when auth.issuer_url is set")
	}
	if (c.Release.Ref == "") != (c.Release.Key == "") {
		return errors.New("release.ref and release.key are set together: release.ref names where the API publishes, release.key signs")
	}
	if c.Release.OverlayRef != "" && c.Release.Ref == "" {
		return errors.New("release.overlay_ref needs release.ref: an overlay is published with its release")
	}
	if u := c.Upstream; u.Ref == "" && u != (UpstreamConfig{}) {
		return errors.New("upstream.* is set without upstream.ref: an overlay and the trust settings apply to the upstream release only")
	}
	if len(c.AzurePrices.Regions) == 0 {
		return errors.New("azure_prices.regions is required")
	}
	for _, r := range c.AzurePrices.Regions {
		if !armRegion.MatchString(r) {
			return fmt.Errorf("azure_prices.regions: %q is not an ARM region name such as eastus (a list takes a TOML file)", r)
		}
	}
	return nil
}

// OverlayDir is the overlay directory NEOCLOUD_CATALOG_CONFIG_DIR names, or "".
func OverlayDir() string {
	return os.Getenv(envPrefix + "CONFIG_DIR")
}
