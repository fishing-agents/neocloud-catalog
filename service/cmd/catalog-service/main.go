// Command catalog-service runs the neocloud catalog service.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"github.com/fishman/neocloud-catalog/service/internal/auth"
	"github.com/fishman/neocloud-catalog/service/internal/config"
	"github.com/fishman/neocloud-catalog/service/internal/db"
	"github.com/fishman/neocloud-catalog/service/internal/httpapi"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/azureprices"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/importers/epoch"
	"github.com/fishman/neocloud-catalog/service/internal/importers/huggingface"
	"github.com/fishman/neocloud-catalog/service/internal/importers/kaito"
	"github.com/fishman/neocloud-catalog/service/internal/importers/upstream"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/release"
	"github.com/fishman/neocloud-catalog/service/internal/server"
	"github.com/fishman/neocloud-catalog/service/internal/store"
	"github.com/fishman/neocloud-catalog/service/migrations"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch cmd := os.Args[1]; {
	case cmd == "serve":
		cmdServe()
	case cmd == "migrate":
		cmdMigrate()
	case cmd == "import" && len(os.Args) == 3:
		cmdImport(os.Args[2])
	case cmd == "generate":
		cmdGenerate()
	case cmd == "release" && len(os.Args) >= 3 && os.Args[2] == "create":
		cmdReleaseCreate(os.Args[3:])
	case cmd == "release" && len(os.Args) >= 3 && os.Args[2] == "publish":
		cmdReleasePublish(os.Args[3:])
	case cmd == "auth" && len(os.Args) >= 3 && os.Args[2] == "issue":
		cmdAuthIssue(os.Args[3:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `catalog-service <command>

commands:
  serve             serve the /v1 API, /healthz and /readyz on server.addr and
                    Prometheus metrics on telemetry.metrics_addr, and work the
                    catalog queue
  migrate           apply the schema with GORM AutoMigrate plus the releases
                    immutability trigger (dev and tests); production applies
                    the versioned Atlas migrations (make migrate-apply)
  import <importer> run an importer as a job on the catalog queue and print its
                    run: bootstrap-from-json (bootstrap.data_dir into the
                    upstream layer), epoch (family hardware facts from Epoch
                    AI's ML hardware data and the curated
                    families.overlay.json in bootstrap.data_dir),
                    huggingface (manifests, weights and model facts of the
                    upstream sources at their pinned revisions; gated
                    repositories need huggingface.token_file),
                    azure-prices (Azure Retail Prices of the azure/ node
                    types in azure_prices.regions, internal), kaito
                    (cross-check of model architectures and context limits
                    against KAITO's model catalog at its pinned release;
                    lists disagreements, writes nothing) or upstream (in an
                    operator deployment, with upstream.ref set: our signed
                    release into the upstream layer and, with
                    upstream.overlay_ref, our signed overlay pinned to it
                    into the overlay layer; refuses a release older than one
                    imported). An operator deployment refuses
                    bootstrap-from-json, epoch and huggingface: its upstream
                    layer is the upstream release's
  generate          run the generator over the effective catalog as a job on
                    the catalog queue, and print its run
  release create <generator-run-id>
                    freeze a succeeded generator run as a release at its
                    generation time, and print the release
  release publish -ref <repository:tag> (-key <cosign.key> | -keyless)
                  [-overlay-ref <repository:tag>] [-exports <dir>] <version>
                    push the release's snapshot to the repository as the
                    signed catalog artifact and point the tag at it (once:
                    again with identical content changes nothing); -key signs
                    with a cosign key pair (password in COSIGN_PASSWORD),
                    -keyless runs cosign's keyless flow (CI); -overlay-ref
                    also pushes the release's overlay, pinned to its digest,
                    as the signed overlay artifact (also later, once, for a
                    release published without it); -exports also writes the
                    adapter exports and a signed SHA256SUMS to dir
  auth issue [-name <name>] [-prefix <prefix>] [-expires <duration>]
                    mint an API key for the /v1 API (neocatalog_<prefix>_...);
                    it is printed once and stored only as its SHA-256

import and generate work the queue themselves until their job finished, and
then until a job of another process they took finished (at most
jobs.job_timeout); a signal cancels their own job only, a second one exits.

config: embedded defaults, then $NEOCLOUD_CATALOG_CONFIG_DIR/*.toml, then
NEOCLOUD_CATALOG_<KEY> env (NEOCLOUD_CATALOG_DATABASE_DSN -> database.dsn)`)
}

func open() (*config.Config, *sql.DB, *gorm.DB) {
	cfg, err := config.Load(config.OverlayDir())
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	sqlDB, gormDB, err := db.Open(cfg.Database.DSN, cfg.Database.MaxConns)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	return cfg, sqlDB, gormDB
}

func cmdServe() {
	cfg, sqlDB, gormDB := open()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var publish *release.Target
	if cfg.Release.Ref != "" {
		signer, err := releaseSigner(cfg.Release.Key, false)
		if err != nil {
			log.Fatalf("release.key: %v", err)
		}
		publish = &release.Target{Ref: cfg.Release.Ref, OverlayRef: cfg.Release.OverlayRef, Signer: signer, PlainHTTP: cfg.Release.PlainHTTP}
	}
	s := store.New(gormDB)
	reg := mustRegistry(cfg)
	if err := jobs.Migrate(ctx, sqlDB); err != nil {
		log.Fatalf("river migrate: %v", err)
	}
	c, err := jobs.New(sqlDB, s, reg, jobsConfig(cfg))
	if err != nil {
		log.Fatalf("jobs: %v", err)
	}
	api := httpapi.New(httpapi.Config{Store: s, Jobs: c, Importers: reg, Publish: publish, PublishTimeout: cfg.Release.PublishTimeout,
		Auth: auth.New(gormDB, auth.OIDCConfig{IssuerURL: cfg.Auth.IssuerURL, Audience: cfg.Auth.Audience})})
	log.Printf("serving on %s (metrics %s)", cfg.Server.Addr, cfg.Telemetry.MetricsAddr)
	err = runServe(ctx, c, cfg.Server.WriteTimeout, func() error {
		return server.Serve(ctx, cfg, server.Handler(sqlDB.PingContext, api))
	})
	if err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// runServe works the queue while serve runs; on shutdown, runs still going get
// grace, then are cancelled and recorded failed instead of holding shutdown.
func runServe(ctx context.Context, c *jobs.Client, grace time.Duration, serve func() error) error {
	// Detached: cancelling River's start context would cancel the jobs at once.
	if err := c.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	return errors.Join(serve(), c.Shutdown(grace))
}

func jobsConfig(cfg *config.Config) jobs.Config {
	return jobs.Config{JobTimeout: cfg.Jobs.JobTimeout, RescueAfter: cfg.Jobs.RescueAfter}
}

func cmdMigrate() {
	_, sqlDB, gormDB := open()
	if err := gormDB.AutoMigrate(model.All()...); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	// One batch, so one transaction: re-running replaces the trigger.
	trigger := `DROP TRIGGER IF EXISTS "releases_immutable" ON "releases"; DROP FUNCTION IF EXISTS "releases_immutable"();` +
		migrations.ReleasesImmutable
	if _, err := sqlDB.Exec(trigger); err != nil {
		log.Fatalf("migrate: releases trigger: %v", err)
	}
	fmt.Println("schema applied")
}

func cmdImport(name string) {
	cfg, sqlDB, gormDB := open()
	reg := mustRegistry(cfg)
	if _, err := reg.Lookup(name); err != nil {
		log.Fatalf("%v; importers: %s", err, strings.Join(reg.Names(), ", "))
	}
	runJob(cfg, sqlDB, store.New(gormDB), reg, jobs.ImportArgs{Importer: name, Run: uuid.New()})
}

func cmdGenerate() {
	cfg, sqlDB, gormDB := open()
	runJob(cfg, sqlDB, store.New(gormDB), mustRegistry(cfg), jobs.GenerateArgs{Run: uuid.New()})
}

func mustRegistry(cfg *config.Config) importers.Registry {
	reg, err := registry(cfg)
	if err != nil {
		log.Fatal(err)
	}
	return reg
}

// registry refuses, in an operator deployment (upstream.ref set), the importers
// writing the upstream layer: there it comes from the upstream release only.
func registry(cfg *config.Config) (importers.Registry, error) {
	prices, cross := azureprices.New(azureprices.Endpoint, cfg.AzurePrices.Regions), kaito.New(kaito.Catalog)
	u := cfg.Upstream
	rel, err := upstream.NewSource(upstream.Config{Ref: u.Ref, IdentityIssuer: u.IdentityIssuer, Identity: u.Identity,
		PublicKey: u.PublicKey, TrustedRoot: u.TrustedRoot, PlainHTTP: u.PlainHTTP})
	if err != nil {
		return importers.Registry{}, err
	}
	if rel != nil {
		overlay, err := upstream.NewSource(upstream.Config{Ref: u.OverlayRef, IdentityIssuer: u.OverlayIdentityIssuer,
			Identity: u.OverlayIdentity, PublicKey: u.OverlayPublicKey, TrustedRoot: u.TrustedRoot, PlainHTTP: u.PlainHTTP, Overlay: true})
		if err != nil {
			return importers.Registry{}, err
		}
		reg := importers.NewRegistry(prices, cross, upstream.New(rel, overlay))
		for _, name := range []string{(*bootstrap.Importer)(nil).Name(), (*epoch.Importer)(nil).Name(), (*huggingface.Importer)(nil).Name()} {
			reg.Refuse(name, "an operator deployment (upstream.ref set) takes its upstream layer from the upstream release only")
		}
		return reg, nil
	}
	boot, err := bootstrap.New(cfg.Bootstrap.DataDir)
	if err != nil {
		return importers.Registry{}, fmt.Errorf("bootstrap.data_dir: %w", err)
	}
	var token string
	if f := cfg.HuggingFace.TokenFile; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return importers.Registry{}, fmt.Errorf("huggingface.token_file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	hardware, err := epoch.New(epoch.CSV, filepath.Join(cfg.Bootstrap.DataDir, "families.overlay.json"))
	if err != nil {
		return importers.Registry{}, fmt.Errorf("bootstrap.data_dir: %w", err)
	}
	return importers.NewRegistry(boot, hardware, huggingface.New(huggingface.Endpoint, token), prices, cross), nil
}

// runJob runs args to completion and prints its recorded output.
func runJob(cfg *config.Config, sqlDB *sql.DB, s *store.Store, reg importers.Registry, args river.JobArgs) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A second signal exits at once; the running job is rescued after jobs.rescue_after.
	context.AfterFunc(ctx, stop)
	out, err := work(ctx, cfg, sqlDB, s, reg, args)
	if err != nil {
		log.Fatalf("%s: %v", args.Kind(), err)
	}
	var b bytes.Buffer
	if err := json.Indent(&b, out, "", "  "); err != nil {
		log.Fatalf("%s output: %v", args.Kind(), err)
	}
	fmt.Println(b.String())
}

// work runs args on the queue it shares with serve's worker. A job of another
// process taken here is not cancelled by ctx, and Stop waits for it.
func work(ctx context.Context, cfg *config.Config, sqlDB *sql.DB, s *store.Store, reg importers.Registry, args river.JobArgs) ([]byte, error) {
	if err := jobs.Migrate(ctx, sqlDB); err != nil {
		return nil, fmt.Errorf("river migrate: %w", err)
	}
	c, err := jobs.New(sqlDB, s, reg, jobsConfig(cfg))
	if err != nil {
		return nil, err
	}
	if err := c.Start(context.WithoutCancel(ctx)); err != nil {
		return nil, err
	}
	defer c.Stop(context.Background())
	res, err := c.Insert(ctx, args, nil)
	if err != nil {
		return nil, err
	}
	out, err := jobs.Wait(ctx, c, res.Job.ID)
	if ctx.Err() != nil {
		_, cancelErr := c.JobCancel(context.WithoutCancel(ctx), res.Job.ID)
		err = errors.Join(err, cancelErr)
	}
	return out, err
}

func cmdReleaseCreate(args []string) {
	if len(args) != 1 {
		usage()
		os.Exit(2)
	}
	id, err := uuid.Parse(args[0])
	if err != nil {
		log.Fatalf("generator run id: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, _, gormDB := open()
	rel, err := release.Create(ctx, store.New(gormDB), id)
	if err != nil {
		log.Fatalf("release create: %v", err)
	}
	printRelease(rel)
}

func cmdReleasePublish(args []string) {
	fs := flag.NewFlagSet("release publish", flag.ExitOnError)
	ref := fs.String("ref", "", "repository and tag clients poll, e.g. ghcr.io/org/neocloud-catalog:v4")
	key := fs.String("key", "", "cosign.key to sign with; its password in COSIGN_PASSWORD")
	keyless := fs.Bool("keyless", false, "sign with cosign's keyless flow (CI)")
	exports := fs.String("exports", "", "directory to write the adapter exports and a signed SHA256SUMS to")
	overlayRef := fs.String("overlay-ref", "", "repository and tag of the release's overlay, e.g. registry.example/org/neocloud-catalog-overlay:v4")
	_ = fs.Parse(args)
	if fs.NArg() != 1 || *ref == "" {
		usage()
		os.Exit(2)
	}
	signer, err := releaseSigner(*key, *keyless)
	if err != nil {
		log.Fatalf("release publish: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, _, gormDB := open()
	s := store.New(gormDB)
	rel, err := release.Get(ctx, s, fs.Arg(0))
	if err != nil {
		log.Fatalf("release publish: %v", err)
	}
	target := release.Target{Ref: *ref, OverlayRef: *overlayRef, Signer: signer, PlainHTTP: cfg.Release.PlainHTTP}
	if err := release.Publish(ctx, rel, target); err != nil {
		log.Fatalf("release publish: %v", err)
	}
	if *exports != "" {
		if err := release.WriteExports(ctx, rel, *exports, signer); err != nil {
			log.Fatalf("release publish: %v", err)
		}
	}
	if rel, err = release.Get(ctx, s, rel.Version); err != nil {
		log.Fatalf("release publish: %v", err)
	}
	printRelease(rel)
}

func cmdAuthIssue(args []string) {
	fs := flag.NewFlagSet("auth issue", flag.ExitOnError)
	name := fs.String("name", "cli-issued", "what the key is for, recorded with it")
	prefix := fs.String("prefix", "key", "label in the key: neocatalog_<prefix>_<secret>")
	expires := fs.Duration("expires", 0, "lifetime of the key; 0 never expires")
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		usage()
		os.Exit(2)
	}
	var expiresAt *time.Time
	if *expires > 0 {
		expiresAt = new(time.Now().Add(*expires))
	}
	_, _, gormDB := open()
	key, _, err := auth.Issue(context.Background(), gormDB, *name, *prefix, expiresAt)
	if err != nil {
		log.Fatalf("auth issue: %v", err)
	}
	fmt.Printf("key: %s\n", key)
	fmt.Println("copy it now - it is shown once and stored only as a hash")
}

func releaseSigner(key string, keyless bool) (release.Signer, error) {
	switch {
	case key != "" && keyless:
		return nil, errors.New("set -key or -keyless, not both")
	case keyless:
		return release.Keyless{}, nil
	case key == "":
		return nil, errors.New("set -key or -keyless")
	}
	pem, err := os.ReadFile(key)
	if err != nil {
		return nil, err
	}
	return release.NewKeySigner(pem, []byte(os.Getenv("COSIGN_PASSWORD")))
}

func printRelease(rel release.Release) {
	b, err := json.MarshalIndent(struct {
		ID            uuid.UUID  `json:"id"`
		Version       string     `json:"version"`
		Digest        string     `json:"digest"`
		SchemaVersion string     `json:"schema_version"`
		GeneratorRun  uuid.UUID  `json:"generator_run"`
		ArtifactRef   string     `json:"artifact_ref,omitempty"`
		OverlayRef    string     `json:"overlay_ref,omitempty"`
		PublishedAt   *time.Time `json:"published_at,omitempty"`
	}{rel.ID, rel.Version, rel.Digest, rel.SchemaVersion, rel.GeneratorRunID, rel.ArtifactRef, rel.OverlayRef, rel.PublishedAt}, "", "  ")
	if err != nil {
		log.Fatalf("release output: %v", err)
	}
	fmt.Println(string(b))
}
