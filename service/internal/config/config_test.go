package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadLayersOverlayThenEnv(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("10-db.toml", "[database]\nmax_conns = 5\ndsn = \"postgres://overlay\"\n")
	write("20-db.toml", "[database]\nmax_conns = 7\n")
	t.Setenv("NEOCLOUD_CATALOG_DATABASE_MAX_CONNS", "9")
	t.Setenv("NEOCLOUD_CATALOG_AZURE_PRICES_REGIONS", "westus3")
	t.Setenv("NEOCLOUD_TELEMETRY_METRICS_ADDR", "127.0.0.1:1") // neocloud-api's prefix, not ours

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.DSN != "postgres://overlay" || cfg.Database.MaxConns != 9 {
		t.Errorf("database = %+v, want overlay dsn and env max_conns 9", cfg.Database)
	}
	if cfg.Telemetry.MetricsAddr != "0.0.0.0:8081" {
		t.Errorf("metrics_addr = %q, want the default", cfg.Telemetry.MetricsAddr)
	}
	if !slices.Equal(cfg.AzurePrices.Regions, []string{"westus3"}) {
		t.Errorf("azure_prices.regions = %q, want env's westus3", cfg.AzurePrices.Regions)
	}
}

func TestLoadRefusesInvalidConfig(t *testing.T) {
	cases := map[string]struct{ env, value, want string }{
		"no dsn":                {"NEOCLOUD_CATALOG_DATABASE_DSN", "", "database.dsn"},
		"metrics on api port":   {"NEOCLOUD_CATALOG_TELEMETRY_METRICS_ADDR", "0.0.0.0:8080", "telemetry.metrics_addr"},
		"zero timeout":          {"NEOCLOUD_CATALOG_SERVER_READ_TIMEOUT", "0s", "server.read_timeout"},
		"zero job timeout":      {"NEOCLOUD_CATALOG_JOBS_JOB_TIMEOUT", "0s", "jobs.job_timeout"},
		"zero publish timeout":  {"NEOCLOUD_CATALOG_RELEASE_PUBLISH_TIMEOUT", "0s", "release.publish_timeout"},
		"rescue before timeout": {"NEOCLOUD_CATALOG_JOBS_RESCUE_AFTER", "1s", "jobs.rescue_after"},
		"issuer, no audience":   {"NEOCLOUD_CATALOG_AUTH_ISSUER_URL", "https://icp.example.com", "auth.audience"},
		"release ref, no key":   {"NEOCLOUD_CATALOG_RELEASE_REF", "ghcr.io/example/catalog:v4", "release.key"},
		"release key, no ref":   {"NEOCLOUD_CATALOG_RELEASE_KEY", "/etc/catalog/cosign.key", "release.ref"},
		"overlay, no release":   {"NEOCLOUD_CATALOG_RELEASE_OVERLAY_REF", "ghcr.io/example/overlay:v4", "release.overlay_ref"},
		"overlay, no upstream":  {"NEOCLOUD_CATALOG_UPSTREAM_OVERLAY_REF", "ghcr.io/example/overlay:v4", "upstream.ref"},
		"key, no upstream":      {"NEOCLOUD_CATALOG_UPSTREAM_PUBLIC_KEY", "/etc/catalog/upstream.pub", "upstream.ref"},
		"plain http, no ref":    {"NEOCLOUD_CATALOG_UPSTREAM_PLAIN_HTTP", "true", "upstream.ref"},
		"region list in env":    {"NEOCLOUD_CATALOG_AZURE_PRICES_REGIONS", "eastus,eastus2", "azure_prices.regions"},
		"quoted region":         {"NEOCLOUD_CATALOG_AZURE_PRICES_REGIONS", "eastus' or 1 eq 1", "azure_prices.regions"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(c.env, c.value)
			if _, err := Load(""); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Load = %v, want an error naming %s", err, c.want)
			}
		})
	}
}
