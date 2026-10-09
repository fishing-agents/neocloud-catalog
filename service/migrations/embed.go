// Package migrations holds the versioned Atlas migrations. The dev schema
// (GORM AutoMigrate) applies the trigger migrations too: GORM cannot express triggers.
package migrations

import _ "embed"

var (
	//go:embed 20261009052754_release_immutable.sql
	releasesImmutable string
	//go:embed 20261009091351_release_overlay_ref_immutable.sql
	releasesOverlayRef string
)

// ReleasesImmutable creates the releases trigger as the migrations leave it.
var ReleasesImmutable = releasesImmutable + releasesOverlayRef
