// Command catalog-service runs the neocloud catalog service.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"github.com/fishman/neocloud-catalog/service/internal/config"
	"github.com/fishman/neocloud-catalog/service/internal/db"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/model"
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
                    migrate-apply)
  import <importer> run an importer (bootstrap-from-json: bootstrap.data_dir
                    into the upstream layer) as a job on the catalog queue,
                    and print its run
  generate          run the generator over the effective catalog as a job on
                    the catalog queue, and print its run

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
