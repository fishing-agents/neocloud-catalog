package catalog

import (
	"fmt"
	"time"
)

// A snapshot is published as a single-layer, cosign-signed OCI artifact; the
// manifest carries the version under VersionAnnotation, so verifying the
// signature also covers it.
const (
	ArtifactType = "application/vnd.neocloud.catalog.v4+json"
	// OverlayArtifactType is pushed to a private registry and signed by its own identity.
	OverlayArtifactType = "application/vnd.neocloud.catalog-overlay.v4+json"
	VersionAnnotation   = "org.opencontainers.image.version"
	// VersionLayout is the generation time in UTC.
	VersionLayout = "2006-01-02T15:04:05Z"
)

// ParseVersion parses a snapshot version. Clients refuse an upstream not newer
// than the catalog in use.
func ParseVersion(v string) (time.Time, error) {
	t, err := time.Parse(VersionLayout, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("catalog version %q is not in the %s layout", v, VersionLayout)
	}
	return t, nil
}
