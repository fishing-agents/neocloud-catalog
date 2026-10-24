// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"

	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

// replaceTemplates makes the templates table hold exactly ts in input order, recording its changes.
// Templates come with the upstream layer only, verbatim: unchanged rows stay.
func replaceTemplates(tx *gorm.DB, ts []catalog.Template, changes Changes) error {
	var have []model.Template
	if err := tx.Find(&have).Error; err != nil {
		return err
	}
	old := make(map[string]model.Template, len(have))
	for _, row := range have {
		old[keyOf(row.Kind, row.Slug, row.Version)] = row
	}
	var c Change
	for i, t := range ts {
		body, err := json.Marshal(t)
		if err != nil {
			return err
		}
		k := keyOf(t.Kind, t.Slug, t.Version)
		row, ok := old[k]
		delete(old, k)
		switch {
		case !ok:
			row = model.Template{Kind: t.Kind, Slug: t.Slug, Version: t.Version, Position: i, Body: string(body)}
			err = tx.Create(&row).Error
			c.Inserted++
		case row.Position != i || row.Body != string(body):
			err = tx.Model(&row).Updates(map[string]any{"position": i, "body": string(body)}).Error
			c.Updated++
		}
		if err != nil {
			return err
		}
	}
	for _, row := range old {
		if err := tx.Delete(&row).Error; err != nil {
			return err
		}
		c.Deleted++
	}
	if c != (Change{}) {
		changes["templates"] = c
	}
	return nil
}

func templates(tx *gorm.DB) ([]catalog.Template, error) {
	var rows []model.Template
	if err := tx.Order("position").Find(&rows).Error; err != nil {
		return nil, err
	}
	var ts []catalog.Template
	for _, row := range rows {
		var t catalog.Template
		if err := json.Unmarshal([]byte(row.Body), &t); err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return ts, nil
}
