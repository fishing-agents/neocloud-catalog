# SPDX-FileCopyrightText: 2026 Reza Jelveh
# SPDX-License-Identifier: Apache-2.0
# Build context is the repository root (go.mod replaces ../ and ../adapters):
# docker build -f service/Dockerfile .
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    cd service && CGO_ENABLED=0 go build -trimpath -o /out/catalog-service ./cmd/catalog-service

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/catalog-service /catalog-service
# Read by bootstrap-from-json and epoch.
COPY data /data
# Applied by the chart's migrate Job, which mounts this image.
COPY service/migrations/*.sql service/migrations/atlas.sum /migrations/
ENV NEOCLOUD_CATALOG_BOOTSTRAP_DATA_DIR=/data
EXPOSE 8080 8081
ENTRYPOINT ["/catalog-service"]
