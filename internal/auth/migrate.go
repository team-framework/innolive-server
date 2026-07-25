package auth

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

func AutoMigrate(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("GORM database is nil")
	}

	if err := db.WithContext(ctx).AutoMigrate(
		&User{},
		&OAuthAccount{},
		&RefreshSession{},
	); err != nil {
		return fmt.Errorf("auto migrate authentication schema: %w", err)
	}

	return nil
}
