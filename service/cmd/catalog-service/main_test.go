// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/elliptic"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sigstore/sigstore/pkg/cryptoutils"

	"github.com/fishman/neocloud-catalog/service/internal/config"
	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// An operator deployment refuses the upstream-layer importers, saying why, and
// needs no bootstrap data; prices and the KAITO cross-check still run.
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

// gate is an importer that runs until open is closed or its context ends.
type gate struct {
	name string
	open chan struct{}
}

func (g gate) Name() string { return g.name }
func (g gate) Origin() importers.Origin {
	return importers.Origin{Layer: model.LayerUpstream, Source: g.name}
}
func (g gate) Run(ctx context.Context, _ *store.Store, _ uuid.UUID) (importers.Summary, error) {
	select {
	case <-g.open:
		return importers.Summary{}, nil
	case <-ctx.Done():
		return importers.Summary{}, ctx.Err()
	}
}

func workSetup(t *testing.T, suffix string, imps ...importers.Importer) (*config.Config, *sql.DB, *store.Store, importers.Registry) {
	t.Helper()
	sqlDB, db := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	if err := jobs.Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Server: config.ServerConfig{WriteTimeout: 100 * time.Millisecond},
		Jobs: config.JobsConfig{JobTimeout: time.Minute, RescueAfter: time.Hour}}
	return cfg, sqlDB, store.New(db), importers.NewRegistry(imps...)
}

func awaitRun(ctx context.Context, t *testing.T, s *store.Store, id uuid.UUID, status string) {
	t.Helper()
	for {
		run, err := s.ImportRun(ctx, id)
		if err == nil && run.Status == status {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("import run %s never %s: %+v %v", id, status, run, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// work shares the queue with serve: a foreign job it took is allowed to finish
// after its own job finishes, not cancelled.
func TestWorkLetsForeignJobsFinish(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	open, quick := make(chan struct{}), make(chan struct{})
	close(quick)
	cfg, sqlDB, s, reg := workSetup(t, "cli_work", gate{"slow", open}, gate{"quick", quick})
	serve, err := jobs.New(sqlDB, s, reg, jobsConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	foreign, own := uuid.New(), uuid.New()
	if _, err := serve.Insert(ctx, jobs.ImportArgs{Importer: "slow", Run: foreign}, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := work(ctx, cfg, sqlDB, s, reg, jobs.ImportArgs{Importer: "quick", Run: own})
		done <- err
	}()
	// The only worker yet is work's: it took the foreign job.
	awaitRun(ctx, t, s, foreign, model.RunRunning)
	if err := serve.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serve.Stop(context.Background()) })
	awaitRun(ctx, t, s, own, model.RunSucceeded)
	time.Sleep(10 * cfg.Server.WriteTimeout)
	if run, err := s.ImportRun(ctx, foreign); err != nil || run.Status != model.RunRunning {
		t.Fatalf("foreign run after work's own job finished: %+v %v, want still running", run, err)
	}
	close(open)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	awaitRun(ctx, t, s, foreign, model.RunSucceeded)
}

// A signal still cancels work's own job, and its run is recorded as failed.
func TestWorkSignalCancelsItsJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, sqlDB, s, reg := workSetup(t, "cli_signal", gate{"slow", make(chan struct{})})
	sig, interrupt := context.WithCancel(ctx)
	own := uuid.New()
	done := make(chan error, 1)
	go func() {
		_, err := work(sig, cfg, sqlDB, s, reg, jobs.ImportArgs{Importer: "slow", Run: own})
		done <- err
	}()
	awaitRun(ctx, t, s, own, model.RunRunning)
	interrupt()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("work after a signal: %v, want context canceled", err)
		}
	case <-ctx.Done():
		t.Fatal("work did not return after a signal")
	}
	if run, err := s.ImportRun(ctx, own); err != nil || run.Status != model.RunFailed {
		t.Errorf("run of the interrupted job: %+v %v, want failed", run, err)
	}
}

// On a signal serve gives running jobs grace: one finishing within it
// succeeds, one overrunning it is cancelled and recorded failed.
func TestServeShutdownGrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	open := make(chan struct{})
	cfg, sqlDB, s, reg := workSetup(t, "serve_grace", gate{"finishes", open}, gate{"overruns", make(chan struct{})})
	serve := func(importer string, grace time.Duration, onSignal func()) model.ImportRun {
		t.Helper()
		c, err := jobs.New(sqlDB, s, reg, jobsConfig(cfg))
		if err != nil {
			t.Fatal(err)
		}
		sig, signal := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- runServe(sig, c, grace, func() error { <-sig.Done(); return nil }) }()
		id := uuid.New()
		if _, err := c.Insert(ctx, jobs.ImportArgs{Importer: importer, Run: id}, nil); err != nil {
			t.Fatal(err)
		}
		awaitRun(ctx, t, s, id, model.RunRunning)
		signal()
		onSignal()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		run, err := s.ImportRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	if run := serve("finishes", 5*time.Second, func() { time.Sleep(200 * time.Millisecond); close(open) }); run.Status != model.RunSucceeded {
		t.Errorf("run finishing within the grace: %s %q, want succeeded", run.Status, run.Error)
	}
	if run := serve("overruns", 200*time.Millisecond, func() {}); run.Status != model.RunFailed || !strings.Contains(run.Error, "context canceled") {
		t.Errorf("run overrunning the grace: %s %q, want failed, cancelled", run.Status, run.Error)
	}
}
