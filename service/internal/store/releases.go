// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/fishman/neocloud-catalog/service/internal/model"
)

// GeneratorRun returns the generator run id.
func (s *Store) GeneratorRun(ctx context.Context, id uuid.UUID) (model.GeneratorRun, error) {
	var run model.GeneratorRun
	err := s.db.WithContext(ctx).Take(&run, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return run, fmt.Errorf("generator run %s: %w", id, ErrNotFound)
	}
	return run, err
}

// CreateRelease stores rel; a duplicate version is ErrConflict.
func (s *Store) CreateRelease(ctx context.Context, rel *model.Release) error {
	err := s.db.WithContext(ctx).Create(rel).Error
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: a release of version %s exists", ErrConflict, rel.Version)
	}
	return err
}

// Releases returns every release, newest first, without its overlay.
func (s *Store) Releases(ctx context.Context) ([]model.Release, error) {
	releases := []model.Release{}
	return releases, s.db.WithContext(ctx).Omit("overlay").Order("version DESC").Find(&releases).Error
}

// Release returns the release of version.
func (s *Store) Release(ctx context.Context, version string) (model.Release, error) {
	var rel model.Release
	err := s.db.WithContext(ctx).Take(&rel, "version = ?", version).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return rel, fmt.Errorf("release %s: %w", version, ErrNotFound)
	}
	return rel, err
}

// NewerPublished returns the newest published version after version, or "".
// Versions are fixed-width UTC times, so string order is time order.
func (s *Store) NewerPublished(ctx context.Context, version string) (string, error) {
	var newer []string
	err := s.db.WithContext(ctx).Model(&model.Release{}).Where("published_at IS NOT NULL AND version > ?", version).
		Order("version DESC").Limit(1).Pluck("version", &newer).Error
	if err != nil || len(newer) == 0 {
		return "", err
	}
	return newer[0], nil
}

// PublishRelease marks release id published as ref with overlay overlayRef ("" for none).
// Republishing as ref is a no-op; as another ref, an error.
func (s *Store) PublishRelease(ctx context.Context, id uuid.UUID, ref, overlayRef string) (model.Release, error) {
	var rel model.Release
	res := s.db.WithContext(ctx).Model(&rel).Clauses(clause.Returning{}).Where("id = ? AND published_at IS NULL", id).
		Updates(map[string]any{"artifact_ref": ref, "overlay_ref": overlayRef, "published_at": now()})
	if res.Error != nil || res.RowsAffected == 1 {
		return rel, res.Error
	}
	if err := s.db.WithContext(ctx).Take(&rel, "id = ?", id).Error; err != nil {
		return rel, err
	}
	if rel.ArtifactRef != ref {
		return rel, fmt.Errorf("release %s is published as %s", rel.Version, rel.ArtifactRef)
	}
	return rel, nil
}

// PublishOverlay records the published release id's overlay as overlayRef, once; another ref is ErrConflict.
func (s *Store) PublishOverlay(ctx context.Context, id uuid.UUID, overlayRef string) error {
	res := s.db.WithContext(ctx).Model(&model.Release{}).Where("id = ? AND published_at IS NOT NULL AND overlay_ref IN ('', ?)", id, overlayRef).
		Update("overlay_ref", overlayRef)
	if res.Error != nil || res.RowsAffected == 1 {
		return res.Error
	}
	return fmt.Errorf("%w: release %s is unpublished or its overlay is published elsewhere", ErrConflict, id)
}
