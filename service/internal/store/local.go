package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

// LocalModel returns the local model slug with its quantizations, sources and
// presets.
func (s *Store) LocalModel(ctx context.Context, slug string) (catalog.ModelTemplate, error) {
	var m catalog.ModelTemplate
	err := s.read(ctx, func(tx *gorm.DB) error {
		var err error
		m, err = localModel(tx, slug)
		return err
	})
	return m, err
}

func localModel(tx *gorm.DB, slug string) (catalog.ModelTemplate, error) {
	o, err := local(tx, scope{KindModel, slug})
	if err != nil {
		return catalog.ModelTemplate{}, err
	}
	if len(o.models) == 0 {
		return catalog.ModelTemplate{}, fmt.Errorf("local model %q: %w", slug, ErrNotFound)
	}
	return o.models[0].obj, nil
}

// LocalNodeType returns the local node type id.
func (s *Store) LocalNodeType(ctx context.Context, id string) (catalog.NodeType, error) {
	var o objects
	err := s.read(ctx, func(tx *gorm.DB) error {
		var err error
		o, err = local(tx, scope{KindNodeType, id})
		return err
	})
	if err != nil {
		return catalog.NodeType{}, err
	}
	if len(o.nodeTypes) == 0 {
		return catalog.NodeType{}, fmt.Errorf("local node type %q: %w", id, ErrNotFound)
	}
	return o.nodeTypes[0].obj, nil
}

func local(tx *gorm.DB, sc scope) (objects, error) {
	rs, err := load(tx, model.LayerLocal, sc)
	if err != nil {
		return objects{}, err
	}
	return assemble(rs)
}

// EditLocalModel applies edit to the local model slug and stores the result
// as Upsert does, under one write lock, so concurrent edits do not lose each
// other's changes. Without a local model it returns ErrNotFound.
func (s *Store) EditLocalModel(ctx context.Context, slug string, edit func(*catalog.ModelTemplate) error) error {
	return s.write(ctx, func(tx *gorm.DB) error {
		m, err := localModel(tx, slug)
		if err != nil {
			return err
		}
		if err := edit(&m); err != nil {
			return err
		}
		return upsert(tx, m)
	})
}

var billings = []string{model.BillingOnDemand, model.BillingSpot, model.BillingReserved1Y}

// LocalPrices returns the local layer's node type prices by node type, region
// and billing.
func (s *Store) LocalPrices(ctx context.Context) ([]model.NodeTypePrice, error) {
	prices := []model.NodeTypePrice{}
	err := s.db.WithContext(ctx).Where("layer = ?", model.LayerLocal).Order("node_type_id, region, billing").Find(&prices).Error
	return prices, err
}

// ReplaceLocalPrices makes the local layer hold exactly the node type prices
// of prices (their NodeTypeID, Region, Billing and USDPerHour), curated now;
// an unchanged price keeps its provenance.
func (s *Store) ReplaceLocalPrices(ctx context.Context, prices []model.NodeTypePrice) error {
	var errs []error
	seen := map[[3]string]bool{}
	want := make([]model.NodeTypePrice, len(prices))
	for i, p := range prices {
		k := [3]string{p.NodeTypeID, p.Region, p.Billing}
		switch {
		case p.NodeTypeID == "" || p.Region == "":
			errs = append(errs, fmt.Errorf("price %d: node type and region required", i))
		case !slices.Contains(billings, p.Billing):
			errs = append(errs, fmt.Errorf("price %d: billing %q, want one of %v", i, p.Billing, billings))
		case p.USDPerHour < 0:
			errs = append(errs, fmt.Errorf("price %d: negative price", i))
		case seen[k]:
			errs = append(errs, fmt.Errorf("price %d: duplicate %s %s %s", i, p.NodeTypeID, p.Region, p.Billing))
		}
		seen[k] = true
		want[i] = model.NodeTypePrice{NodeTypeID: p.NodeTypeID, Region: p.Region, Billing: p.Billing, USDPerHour: p.USDPerHour}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalid, errors.Join(errs...))
	}
	return s.write(ctx, func(tx *gorm.DB) error {
		var have []model.NodeTypePrice
		if err := tx.Where("layer = ?", model.LayerLocal).Find(&have).Error; err != nil {
			return err
		}
		_, err := syncTable(tx, stamp{layer: model.LayerLocal, source: model.SourceCuration, asOf: now()}, have, want,
			func(r *model.NodeTypePrice) string { return keyOf(r.NodeTypeID, r.Region, r.Billing) })
		return err
	})
}
