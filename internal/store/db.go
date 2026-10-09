// package store：MySQL 连接、幂等迁移、会话 / 权限 / 审计的持久化。
//
// ★ 驱动用纯 Go 的 github.com/go-sql-driver/mysql —— 不引入 cgo（任务包 D3）。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Store 是数据访问入口（一个进程一个实例即可）。
type Store struct {
	db *sql.DB
	// retention 是留样保留期限的生效配置（零值 ⇒ 用 D4 安全缺省，见 retention.go）。
	retention RetentionDefaults
}

// Open 建立连接池并做一次 PING（连不上 ⇒ 立刻返回错误，不静默降级）。
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库连接失败: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库 PING 失败: %w", err)
	}
	return &Store{db: db}, nil
}

// DB 暴露底层句柄（迁移与测试用）。
func (s *Store) DB() *sql.DB { return s.db }

// Close 关闭连接池。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
