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

	if err := dropOAuthUserProviderUnique(ctx, db); err != nil {
		return err
	}
	if err := dropStreamingUserProviderUnique(ctx, db); err != nil {
		return err
	}
	if err := db.WithContext(ctx).AutoMigrate(
		&User{},
		&OAuthAccount{},
		&EmailAccount{},
		&RefreshSession{},
		&StreamingAccount{},
	); err != nil {
		return fmt.Errorf("auto migrate authentication schema: %w", err)
	}

	return nil
}

// dropOAuthUserProviderUnique는 사용자·공급자 1:1 제약을 지운다(#392). AutoMigrate는
// 기존 인덱스를 지우지 않는다. SQL 마이그레이션은 제약으로, AutoMigrate는 고유
// 인덱스로 만들었으므로 두 형태를 모두 지운다. 애플 1개 제약은 AutoMigrate가
// 부분 고유 인덱스로 다시 만든다.
func dropOAuthUserProviderUnique(ctx context.Context, db *gorm.DB) error {
	if !db.Migrator().HasTable(&OAuthAccount{}) {
		return nil
	}
	for _, statement := range []string{
		"ALTER TABLE oauth_accounts DROP CONSTRAINT IF EXISTS uidx_oauth_user_provider",
		"DROP INDEX IF EXISTS uidx_oauth_user_provider",
	} {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("drop oauth user provider unique: %w", err)
		}
	}
	return nil
}

// dropStreamingUserProviderUnique는 송출 연결의 사용자·플랫폼 1:1 제약을 지운다(#390).
// 유튜브는 채널 단위로 여러 개, 치지직은 1개라 AutoMigrate가 플랫폼별 부분 고유
// 인덱스로 다시 만든다. 제약·인덱스 두 형태를 모두 지우는 이유는 위와 같다.
func dropStreamingUserProviderUnique(ctx context.Context, db *gorm.DB) error {
	if !db.Migrator().HasTable(&StreamingAccount{}) {
		return nil
	}
	for _, statement := range []string{
		"ALTER TABLE streaming_accounts DROP CONSTRAINT IF EXISTS uidx_streaming_user_provider",
		"DROP INDEX IF EXISTS uidx_streaming_user_provider",
	} {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("drop streaming user provider unique: %w", err)
		}
	}
	return nil
}
