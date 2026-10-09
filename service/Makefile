GO ?= go
ATLAS ?= atlas
NAME ?= change
TEST_DSN ?= postgres://neocloud:neocloud@localhost:5432/catalog_service_test?sslmode=disable

.PHONY: all build generate vet fmt lint test test-unit test-integration migrate migrate-diff migrate-apply serve

all: build

build:
	$(GO) build ./...

generate:
	$(GO) run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 -config api/codegen.yaml api/openapi.yaml

vet:
	$(GO) vet ./...
	$(GO) vet -tags atlas ./tools/atlas

fmt:
	gofmt -w ./cmd ./internal ./tools

lint: vet
	@test -z "$$(gofmt -l ./cmd ./internal ./tools)" || (echo "gofmt needed:"; gofmt -l ./cmd ./internal ./tools; exit 1)

test: test-unit test-integration

test-unit:
	$(GO) test ./...

# Each DB test creates and drops its own <database>_<suffix> next to TEST_DSN's.
test-integration:
	NEOCLOUD_CATALOG_TEST_DSN="$(TEST_DSN)" $(GO) test ./... -count=1

# Dev/test schema: GORM AutoMigrate, runnable without the atlas binary.
migrate:
	$(GO) run ./cmd/catalog-service migrate

# Production schema: Atlas versioned migrations (migrations/). The GORM models
# are the schema authority; migrate-diff writes the next file from them against
# a docker postgres:16 dev database (see atlas.hcl).
migrate-diff:
	$(ATLAS) migrate diff $(NAME) --env gorm

migrate-apply:
	$(ATLAS) migrate apply --env gorm --url "$${NEOCLOUD_CATALOG_DATABASE_DSN}"

serve:
	$(GO) run ./cmd/catalog-service serve
