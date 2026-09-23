package migration

import (
	"context"
	"errors"
	"fmt"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/config"
	"inno-live-server/internal/usage"

	"gorm.io/gorm"
)

func Run(
	ctx context.Context,
	db *gorm.DB,
	databaseURL string,
	mode config.DatabaseMigrationMode,
) error {
	if db == nil {
		return errors.New("GORM database is nil")
	}

	switch mode {
	case config.DatabaseMigrationModeAuto:
		if err := auth.AutoMigrate(ctx, db); err != nil {
			return err
		}
		return usage.AutoMigrate(ctx, db)

	case config.DatabaseMigrationModeVersioned:
		return runVersioned(databaseURL)

	case config.DatabaseMigrationModeOff:
		return nil

	default:
		return fmt.Errorf(
			"unsupported database migration mode %q",
			mode,
		)
	}
}
