# SPDX-License-Identifier: Apache-2.0
GO ?= go
ATLAS ?= atlas
NAME ?= change
TEST_DSN ?= postgres://neocloud:neocloud@localhost:5432/catalog_service_test?sslmode=disable

.PHONY: all build generate vet fmt lint test test-unit test-integration chart-test compose-test migrate migrate-diff migrate-apply serve image

all: build

build:
	$(GO) build ./...

generate:
	$(GO) run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 -config api/codegen.yaml api/openapi.yaml

vet:
	$(GO) vet ./...
	$(GO) vet -tags atlas ./tools/atlas

fmt:
	gofmt -w ./cmd ./internal ./tools ./charts

lint: vet
	@test -z "$$(gofmt -l ./cmd ./internal ./tools ./charts)" || (echo "gofmt needed:"; gofmt -l ./cmd ./internal ./tools ./charts; exit 1)

test: test-unit test-integration

test-unit:
	$(GO) test ./...

# Skipped without helm.
chart-test:
	$(GO) test ./charts/catalog-service/tests/ -count=1

compose-test:
	deploy/test.sh && deploy/test.sh down

# Each DB test creates and drops its own database next to TEST_DSN's.
test-integration:
	NEOCLOUD_CATALOG_TEST_DSN="$(TEST_DSN)" $(GO) test ./... -count=1

# Dev/test schema; no atlas binary needed.
migrate:
	$(GO) run ./cmd/catalog-service migrate

# Production schema: Atlas diffs the GORM models into the next migration
# against a docker postgres:16 dev database (atlas.hcl).
migrate-diff:
	$(ATLAS) migrate diff $(NAME) --env gorm

migrate-apply:
	$(ATLAS) migrate apply --env gorm --url "$${NEOCLOUD_CATALOG_DATABASE_DSN}"

serve:
	$(GO) run ./cmd/catalog-service serve

# Build context is the repository root (go.mod replaces ../ and ../adapters).
image:
	docker build -f Dockerfile -t catalog-service ..
