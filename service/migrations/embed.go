// Package migrations holds the versioned Atlas migrations. ReleasesImmutable
// is the hand-written trigger migration, which the dev schema (GORM
// AutoMigrate) applies too, as GORM cannot express triggers.
package migrations

import _ "embed"

//go:embed 20261009052754_release_immutable.sql
var ReleasesImmutable string
