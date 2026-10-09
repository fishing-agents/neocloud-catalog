// Command catalog-service runs the neocloud catalog service.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"gorm.io/gorm"

	"github.com/fishman/neocloud-catalog/service/internal/config"
	"github.com/fishman/neocloud-catalog/service/internal/db"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe()
	case "migrate":
		cmdMigrate()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `catalog-service <command>

commands:
  serve     serve /healthz and /readyz on server.addr and Prometheus metrics
            on telemetry.metrics_addr
  migrate   apply the schema with GORM AutoMigrate (dev and tests); production
            applies the versioned Atlas migrations (make migrate-apply)

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
