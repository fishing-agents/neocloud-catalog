# Static, non-root distroless image. The service module replaces the core and
# adapters modules with ../ and ../adapters, so the build context is the
# repository root: docker build -f service/Dockerfile .
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    cd service && CGO_ENABLED=0 go build -trimpath -o /out/catalog-service ./cmd/catalog-service

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/catalog-service /catalog-service
# bootstrap-from-json and epoch read the curated data/ the image was built from.
COPY data /data
# The versioned Atlas migrations; the chart's migrate Job mounts this image
# and applies them with the atlas image.
COPY service/migrations/*.sql service/migrations/atlas.sum /migrations/
ENV NEOCLOUD_CATALOG_BOOTSTRAP_DATA_DIR=/data
EXPOSE 8080 8081
ENTRYPOINT ["/catalog-service"]
