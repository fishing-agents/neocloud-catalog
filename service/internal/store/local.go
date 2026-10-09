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

// LocalModel returns the local model slug with its children.
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

// EditLocalModel applies edit to the local model slug under the write lock, so concurrent edits are not lost.
// Without a local model it returns ErrNotFound.
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

// LocalPrices returns the local layer's node type prices.
func (s *Store) LocalPrices(ctx context.Context) ([]model.NodeTypePrice, error) {
	prices := []model.NodeTypePrice{}
	err := s.db.WithContext(ctx).Where("layer = ?", model.LayerLocal).Order("node_type_id, region, billing").Find(&prices).Error
	return prices, err
}

// EffectivePrices returns the effective node type prices: a local price overrides the upstream one of its key.
func (s *Store) EffectivePrices(ctx context.Context) ([]model.NodeTypePrice, error) {
	var prices []model.NodeTypePrice
	// "local" sorts before "upstream": a key's first row is its effective one.
	err := s.db.WithContext(ctx).Where("layer IN ?", []string{model.LayerLocal, model.LayerUpstream}).
		Order("node_type_id, region, billing, layer").Find(&prices).Error
	return slices.CompactFunc(prices, func(a, b model.NodeTypePrice) bool { return priceKey(&a) == priceKey(&b) }), err
}

// ReplaceLocalPrices makes the local layer hold exactly prices; an unchanged price keeps its provenance.
func (s *Store) ReplaceLocalPrices(ctx context.Context, prices []model.NodeTypePrice) error {
	want, errs := checkPrices(prices)
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalid, errors.Join(errs...))
	}
	return s.write(ctx, func(tx *gorm.DB) error {
		var have []model.NodeTypePrice
		if err := tx.Where("layer = ?", model.LayerLocal).Find(&have).Error; err != nil {
			return err
		}
		_, _, err := syncTable(tx, stamp{layer: model.LayerLocal, source: model.SourceCuration, asOf: now()}, have, want, priceKey)
		return err
	})
}

// checkPrices returns prices reduced to their key and USDPerHour, and why it refuses any of them.
func checkPrices(prices []model.NodeTypePrice) ([]model.NodeTypePrice, []error) {
	var errs []error
	seen := map[string]bool{}
	want := make([]model.NodeTypePrice, len(prices))
	for i, p := range prices {
		want[i] = model.NodeTypePrice{NodeTypeID: p.NodeTypeID, Region: p.Region, Billing: p.Billing, USDPerHour: p.USDPerHour}
		k := priceKey(&want[i])
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
	}
	return want, errs
}

func priceKey(p *model.NodeTypePrice) string { return keyOf(p.NodeTypeID, p.Region, p.Billing) }
