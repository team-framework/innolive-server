package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const connectTimeout = 5 * time.Second

type Options struct {
	URL string

	MaxOpenConns int
	MaxIdleConns int

	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration

	Debug bool
}

type Connection struct {
	DB    *gorm.DB
	sqlDB *sql.DB
}

func Open(ctx context.Context, options Options) (*Connection, error) {
	if options.URL == "" {
		return nil, errors.New("database URL is empty")
	}
	if options.MaxOpenConns < 1 {
		return nil, errors.New("database max open connections must be at least 1")
	}
	if options.MaxIdleConns < 0 {
		return nil, errors.New("database max idle connections must not be negative")
	}
	if options.MaxIdleConns > options.MaxOpenConns {
		return nil, errors.New("database max idle connections must not exceed max open connections")
	}
	if options.ConnMaxLifetime <= 0 {
		return nil, errors.New("database connection max lifetime must be positive")
	}
	if options.ConnMaxIdleTime <= 0 {
		return nil, errors.New("database connection max idle time must be positive")
	}

	logMode := gormlogger.Warn
	if options.Debug {
		logMode = gormlogger.Info
	}

	gormDB, err := gorm.Open(postgres.Open(options.URL), &gorm.Config{
		// PostgreSQL의 unique violation 등을
		// gorm.ErrDuplicatedKey 같은 공통 에러로 변환한다.
		TranslateError: true,

		Logger: gormlogger.Default.LogMode(logMode),

		// false가 기본값이다.
		// 인증 데이터의 일관성을 위해 GORM 기본 transaction을 유지한다.
		SkipDefaultTransaction: false,
	})
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL with GORM: %w", err)
	}

	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("get underlying SQL database: %w", err)
	}

	sqlDB.SetMaxOpenConns(options.MaxOpenConns)
	sqlDB.SetMaxIdleConns(options.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(options.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(options.ConnMaxIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	return &Connection{
		DB:    gormDB,
		sqlDB: sqlDB,
	}, nil
}

func (c *Connection) Close() error {
	if c == nil || c.sqlDB == nil {
		return nil
	}
	return c.sqlDB.Close()
}

func (c *Connection) Ping(ctx context.Context) error {
	if c == nil || c.sqlDB == nil {
		return errors.New("database connection is nil")
	}
	return c.sqlDB.PingContext(ctx)
}
