package catalog

import (
	"fmt"
	"time"
)

// A published snapshot is a single-layer OCI artifact: the manifest's artifact
// type and the layer's media type are ArtifactType, the layer is the snapshot
// JSON, and the manifest carries the snapshot version under VersionAnnotation.
// The release workflow signs the manifest with cosign (keyless), so a client
// that verifies the signature against the manifest digest also trusts the
// annotation and the layer digest.
const (
	ArtifactType = "application/vnd.neocloud.catalog.v4+json"
	// OverlayArtifactType is the artifact and layer type of a published
	// Overlay, pushed to a private registry and signed by its own identity.
	OverlayArtifactType = "application/vnd.neocloud.catalog-overlay.v4+json"
	VersionAnnotation   = "org.opencontainers.image.version"
	// VersionLayout is the snapshot version format: the generation time in UTC.
	VersionLayout = "2006-01-02T15:04:05Z"
)

// ParseVersion parses a snapshot version. Clients order catalogs by it and
// refuse an upstream that is not newer than the catalog in use.
func ParseVersion(v string) (time.Time, error) {
	t, err := time.Parse(VersionLayout, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("catalog version %q is not in the %s layout", v, VersionLayout)
	}
	return t, nil
}
