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
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"github.com/fishman/neocloud-catalog/service/internal/config"
	"github.com/fishman/neocloud-catalog/service/internal/db"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/release"
	"github.com/fishman/neocloud-catalog/service/internal/server"
	"github.com/fishman/neocloud-catalog/service/internal/store"
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
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `catalog-service <command>

commands:
  serve             serve /healthz and /readyz on server.addr and Prometheus
                    metrics on telemetry.metrics_addr
  migrate           apply the schema with GORM AutoMigrate (dev and tests);
                    production applies the versioned Atlas migrations (make
                    migrate-apply), which alone install the trigger keeping
                    releases immutable
  import <importer> run an importer (bootstrap-from-json: bootstrap.data_dir
                    into the upstream layer) as a job on the catalog queue,
                    and print its run
  generate          run the generator over the effective catalog as a job on
                    the catalog queue, and print its run
  release create <generator-run-id>
                    freeze a succeeded generator run as a release at its
                    generation time, and print the release
  release publish -ref <repository:tag> (-key <cosign.key> | -keyless)
                  [-exports <dir>] <version>
                    push the release's snapshot to the repository as the
                    signed catalog artifact and point the tag at it (once:
                    again with identical content changes nothing); -key signs
                    with a cosign key pair (password in COSIGN_PASSWORD),
                    -keyless runs cosign's keyless flow (CI); -exports also
                    writes the adapter exports and a signed SHA256SUMS to dir

import and generate work the queue themselves until their job finished.

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
	cfg, sqlDB, _ := open()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("serving on %s (metrics %s)", cfg.Server.Addr, cfg.Telemetry.MetricsAddr)
	if err := server.Serve(ctx, cfg, server.Handler(sqlDB.PingContext)); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func cmdMigrate() {
	_, _, gormDB := open()
	if err := gormDB.AutoMigrate(model.All()...); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	fmt.Println("schema applied")
}

func cmdImport(name string) {
	cfg, sqlDB, gormDB := open()
	reg := registry(cfg)
	if reg[name] == nil {
		log.Fatalf("no importer %q; importers: %s", name, strings.Join(slices.Sorted(maps.Keys(reg)), ", "))
	}
	runJob(sqlDB, store.New(gormDB), reg, jobs.ImportArgs{Importer: name})
}

func cmdGenerate() {
	cfg, sqlDB, gormDB := open()
	runJob(sqlDB, store.New(gormDB), registry(cfg), jobs.GenerateArgs{})
}

func registry(cfg *config.Config) importers.Registry {
	boot, err := bootstrap.New(cfg.Bootstrap.DataDir)
	if err != nil {
		log.Fatalf("bootstrap.data_dir: %v", err)
	}
	return importers.NewRegistry(boot)
}

// runJob enqueues args and works the catalog queue until the job finished,
// then prints the output it recorded.
func runJob(sqlDB *sql.DB, s *store.Store, reg importers.Registry, args river.JobArgs) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out, err := work(ctx, sqlDB, s, reg, args)
	if err != nil {
		log.Fatalf("%s: %v", args.Kind(), err)
	}
	var b bytes.Buffer
	if err := json.Indent(&b, out, "", "  "); err != nil {
		log.Fatalf("%s output: %v", args.Kind(), err)
	}
	fmt.Println(b.String())
}

func work(ctx context.Context, sqlDB *sql.DB, s *store.Store, reg importers.Registry, args river.JobArgs) ([]byte, error) {
	if err := jobs.Migrate(ctx, sqlDB); err != nil {
		return nil, fmt.Errorf("river migrate: %w", err)
	}
	c, err := jobs.New(sqlDB, s, reg)
	if err != nil {
		return nil, err
	}
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	defer c.Stop(context.WithoutCancel(ctx))
	res, err := c.Insert(ctx, args, nil)
	if err != nil {
		return nil, err
	}
	return jobs.Wait(ctx, c, res.Job.ID)
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
	_, _, gormDB := open()
	s := store.New(gormDB)
	rel, err := release.Get(ctx, s, fs.Arg(0))
	if err != nil {
		log.Fatalf("release publish: %v", err)
	}
	if err := release.Publish(ctx, rel, release.Target{Ref: *ref, Signer: signer}); err != nil {
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
		PublishedAt   *time.Time `json:"published_at,omitempty"`
	}{rel.ID, rel.Version, rel.Digest, rel.SchemaVersion, rel.GeneratorRunID, rel.ArtifactRef, rel.PublishedAt}, "", "  ")
	if err != nil {
		log.Fatalf("release output: %v", err)
	}
	fmt.Println(string(b))
}
