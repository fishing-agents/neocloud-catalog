package main

import (
	"crypto/elliptic"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sigstore/sigstore/pkg/cryptoutils"

	"github.com/fishman/neocloud-catalog/service/internal/config"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// An operator deployment (upstream.ref set) takes its upstream layer from the
// upstream release only: it refuses the importers that write that layer,
// saying why, and needs no bootstrap data; prices and the KAITO cross-check
// still run (R26).
func TestOperatorDeploymentRefusesUpstreamLayerImporters(t *testing.T) {
	_, pub, err := cryptoutils.GeneratePEMEncodedECDSAKeyPair(elliptic.P256(), cryptoutils.StaticPasswordFunc(nil))
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "upstream.pub")
	if err := os.WriteFile(key, pub, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AzurePrices: config.AzurePricesConfig{Regions: []string{"eastus"}},
		Bootstrap: config.BootstrapConfig{DataDir: filepath.Join(t.TempDir(), "absent")},
		Upstream:  config.UpstreamConfig{Ref: "registry.example/neocloud/catalog:v4", PublicKey: key}}
	reg, err := registry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reg.Names(), []string{"azure-prices", "kaito", "upstream"}; !slices.Equal(got, want) {
		t.Errorf("operator importers %v, want %v", got, want)
	}
	for _, name := range []string{"bootstrap-from-json", "epoch", "huggingface"} {
		if _, err := reg.Lookup(name); !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "upstream.ref") {
			t.Errorf("%s in an operator deployment: %v, want a conflict naming upstream.ref", name, err)
		}
	}

	cfg.Upstream, cfg.Bootstrap.DataDir = config.UpstreamConfig{}, "../../../data"
	if reg, err = registry(cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := reg.Names(), []string{"azure-prices", "bootstrap-from-json", "epoch", "huggingface", "kaito"}; !slices.Equal(got, want) {
		t.Errorf("importers %v, want %v", got, want)
	}
	if _, err := reg.Lookup("upstream"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("upstream without upstream.ref: %v, want not found", err)
	}
}
