package migration

import (
	"context"
	"errors"
	"fmt"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/config"

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
		return auth.AutoMigrate(ctx, db)

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
