package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
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

// CreateRelease stores rel.
func (s *Store) CreateRelease(ctx context.Context, rel *model.Release) error {
	return s.db.WithContext(ctx).Create(rel).Error
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

// PublishRelease records that the release id was published as ref, now. A
// release published as ref already stays as it is; one published as another
// ref is an error.
func (s *Store) PublishRelease(ctx context.Context, id uuid.UUID, ref string) (model.Release, error) {
	var rel model.Release
	res := s.db.WithContext(ctx).Model(&rel).Clauses(clause.Returning{}).Where("id = ? AND published_at IS NULL", id).
		Updates(map[string]any{"artifact_ref": ref, "published_at": now()})
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
