package migration

import (
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed sql/*.sql
var migrationFiles embed.FS

func runVersioned(databaseURL string) (runErr error) {
	if databaseURL == "" {
		return errors.New("database URL is empty")
	}

	sourceDriver, err := iofs.New(migrationFiles, "sql")
	if err != nil {
		return fmt.Errorf("create embedded migration source: %w", err)
	}

	// GORM이 사용하는 연결 풀과 분리된 임시 연결로 migration을 실행한다.
	runner, err := migrate.NewWithSourceInstance(
		"iofs",
		sourceDriver,
		databaseURL,
	)
	if err != nil {
		return fmt.Errorf("create migration runner: %w", err)
	}

	defer func() {
		sourceErr, databaseErr := runner.Close()

		if runErr != nil {
			return
		}
		if sourceErr != nil {
			runErr = fmt.Errorf(
				"close migration source: %w",
				sourceErr,
			)
			return
		}
		if databaseErr != nil {
			runErr = fmt.Errorf(
				"close migration database: %w",
				databaseErr,
			)
		}
	}()

	if err := runner.Up(); err != nil &&
		!errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf(
			"apply versioned migrations: %w",
			err,
		)
	}

	return nil
}
