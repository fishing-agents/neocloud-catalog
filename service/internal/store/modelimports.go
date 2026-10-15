// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/fishman/neocloud-catalog/service/internal/model"
)

// Enqueue inserts a job in the transaction of the state change that needs it.
type Enqueue func(*sql.Tx) error

func enqueueIn(tx *gorm.DB, enqueue Enqueue) error {
	if enqueue == nil {
		return nil
	}
	sqlTx, ok := tx.Statement.ConnPool.(*sql.Tx)
	if !ok {
		return fmt.Errorf("store: transaction is a %T, not a *sql.Tx", tx.Statement.ConnPool)
	}
	return enqueue(sqlTx)
}

// CreateModelImport records mi with enqueue (nil for none) in one transaction, so a queued import has its job.
func (s *Store) CreateModelImport(ctx context.Context, mi *model.ModelImport, enqueue Enqueue) error {
	if mi.Report == nil {
		mi.Report = []string{}
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(mi).Error; err != nil {
			return err
		}
		return enqueueIn(tx, enqueue)
	})
}

// ModelImport returns the model import id.
func (s *Store) ModelImport(ctx context.Context, id uuid.UUID) (model.ModelImport, error) {
	var mi model.ModelImport
	err := s.db.WithContext(ctx).Take(&mi, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return mi, fmt.Errorf("model import %s: %w", id, ErrNotFound)
	}
	return mi, err
}

// ModelImportFilter narrows ModelImports; an empty field matches all.
type ModelImportFilter struct {
	TenantID, State string
	Limit           int
}

// ModelImports returns the newest f.Limit model imports f matches.
func (s *Store) ModelImports(ctx context.Context, f ModelImportFilter) ([]model.ModelImport, error) {
	q := s.db.WithContext(ctx).Order("created_at DESC, id").Limit(f.Limit)
	if f.TenantID != "" {
		q = q.Where("tenant_id = ?", f.TenantID)
	}
	if f.State != "" {
		q = q.Where("state = ?", f.State)
	}
	imports := []model.ModelImport{}
	return imports, q.Find(&imports).Error
}

// DecideModelImport approves the requested import id (queued, with enqueue in the transaction) or
// rejects it with reason; any other state is ErrConflict.
func (s *Store) DecideModelImport(ctx context.Context, id uuid.UUID, approve bool, reason string, enqueue Enqueue) (model.ModelImport, error) {
	var mi model.ModelImport
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&mi, "id = ?", id).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("model import %s: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if mi.State != model.ImportRequested {
			return fmt.Errorf("%w: model import %s is %s, not requested", ErrConflict, id, mi.State)
		}
		t := now()
		mi.State, mi.Reason, mi.DecidedAt = model.ImportRejected, reason, &t
		if approve {
			mi.State, mi.Reason = model.ImportQueued, ""
		}
		if err := tx.Model(&mi).Select("state", "reason", "decided_at").Updates(&mi).Error; err != nil {
			return err
		}
		if !approve {
			return nil
		}
		return enqueueIn(tx, enqueue)
	})
	return mi, err
}

// StartModelImport moves the queued import id to running; any other state is ErrConflict.
func (s *Store) StartModelImport(ctx context.Context, id uuid.UUID) (model.ModelImport, error) {
	var mi model.ModelImport
	res := s.db.WithContext(ctx).Model(&mi).Clauses(clause.Returning{}).Where("id = ? AND state = ?", id, model.ImportQueued).
		Updates(map[string]any{"state": model.ImportRunning, "started_at": now()})
	if res.Error == nil && res.RowsAffected == 0 {
		return mi, fmt.Errorf("%w: model import %s is not queued", ErrConflict, id)
	}
	return mi, res.Error
}

// FinishModelImport records the end of the running import mi.ID as mi's state (succeeded or failed),
// commit, model slug, report and error; a succeeded private import stores its tenant model tm in the
// same transaction.
func (s *Store) FinishModelImport(ctx context.Context, mi model.ModelImport, tm *model.TenantModel) (model.ModelImport, error) {
	if mi.Report == nil {
		mi.Report = []string{}
	}
	var out model.ModelImport
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&out).Clauses(clause.Returning{}).Where("id = ? AND state = ?", mi.ID, model.ImportRunning).
			Updates(map[string]any{"state": mi.State, "commit": mi.Commit, "model_slug": mi.ModelSlug, "report": mi.Report,
				"error": mi.Error, "finished_at": now()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("model import %s is not running", mi.ID)
		}
		if tm == nil || mi.State != model.ImportSucceeded {
			return nil
		}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}, {Name: "slug"}},
			DoUpdates: clause.AssignmentColumns([]string{"updated_at", "source", "source_url", "as_of", "model_import_id", "model", "fits"})}).
			Create(tm).Error
	})
	return out, err
}

// TenantModels returns tenant's models by slug.
func (s *Store) TenantModels(ctx context.Context, tenant string) ([]model.TenantModel, error) {
	models := []model.TenantModel{}
	return models, s.db.WithContext(ctx).Where("tenant_id = ?", tenant).Order("slug").Find(&models).Error
}
