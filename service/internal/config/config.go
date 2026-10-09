// Package config loads the service configuration: embedded TOML defaults, an
// optional overlay directory of *.toml merged in lexical order (later wins),
// then NEOCLOUD_CATALOG_<KEY> env overrides matched to known keys
// (NEOCLOUD_CATALOG_DATABASE_MAX_CONNS -> database.max_conns).
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

// BootstrapConfig locates the core repository's data/*.json the bootstrap
// importer reads; relative to the working directory.
type BootstrapConfig struct {
	DataDir string `koanf:"data_dir"`
}

// HuggingFaceConfig: TokenFile holds the Hugging Face token (a mounted
// secret) the huggingface importer reads gated repositories with, read once
// at startup (rotation needs a restart); empty reads public metadata only.
type HuggingFaceConfig struct {
	TokenFile string `koanf:"token_file"`
}

// AzurePricesConfig: Regions are the Azure regions (ARM names) the
// azure-prices importer reads prices in.
type AzurePricesConfig struct {
	Regions []string `koanf:"regions"`
}

// armRegion is the form of an ARM region name; it keeps the importer's OData
// filter well formed.
var armRegion = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// AuthConfig: API keys always work; an issuer_url (ICP) also enables OIDC
// JWTs whose aud must contain audience.
type AuthConfig struct {
	IssuerURL string `koanf:"issuer_url"`
	Audience  string `koanf:"audience"`
}

// JobsConfig bounds runs on the catalog queue: JobTimeout cancels a run that
// takes longer; RescueAfter (> JobTimeout) is when a run left running by a
// worker that exited is failed and its job rescued.
type JobsConfig struct {
	JobTimeout  time.Duration `koanf:"job_timeout"`
	RescueAfter time.Duration `koanf:"rescue_after"`
}

// ReleaseConfig is where the API publishes releases: Ref names the
// repository and tag, Key the cosign.key signing them (its password in
// COSIGN_PASSWORD). Both empty: the API does not publish. OverlayRef, when
// set, is where each release's overlay is published, signed with Key.
// PublishTimeout bounds one publish request, which outlasts
// server.write_timeout.
type ReleaseConfig struct {
	Ref            string        `koanf:"ref"`
	Key            string        `koanf:"key"`
	OverlayRef     string        `koanf:"overlay_ref"`
	PublishTimeout time.Duration `koanf:"publish_timeout"`
}

// UpstreamConfig is the upstream release an operator deployment imports
// (the upstream importer), trusted as neocloud-api's catalog.* keys trust
// it: Ref with PublicKey, or with IdentityIssuer and Identity (keyless).
// OverlayRef, with its own trust, is the overlay pinned to that release.
// Empty Ref: no upstream importer.
type UpstreamConfig struct {
	Ref                   string `koanf:"ref"`
	IdentityIssuer        string `koanf:"identity_issuer"`
	Identity              string `koanf:"identity"`
	PublicKey             string `koanf:"public_key"`
	OverlayRef            string `koanf:"overlay_ref"`
	OverlayIdentityIssuer string `koanf:"overlay_identity_issuer"`
	OverlayIdentity       string `koanf:"overlay_identity"`
	OverlayPublicKey      string `koanf:"overlay_public_key"`
}

// applyEnv sets NEOCLOUD_CATALOG_<NAME> on the known key whose path, with "."
// and "_" removed, equals NAME without "_" (case-insensitive). A plain "_" ->
// "." mapping cannot reach keys that contain underscores. Names matching no
// known key, or more than one, are ignored rather than guessed.
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

// Load builds the merged, validated Config. overlayDir is optional; pass "" to
// skip.
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
	if c.Upstream.OverlayRef != "" && c.Upstream.Ref == "" {
		return errors.New("upstream.overlay_ref needs upstream.ref: an overlay is imported with the release it is pinned to")
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
