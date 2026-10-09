// Command jxlabtrace —— 「实验检测数据追踪系统」服务端入口（M0 地基）。
//
// 启动流程：读配置（环境变量 / .env）→ 校验（缺 JX_DB_DSN 即拒绝启动）→
// 连库 → **自动迁移**（幂等）→ 起 HTTP 服务。
//
// 子命令：
//
//	-version   打印版本
//	-migrate   只跑迁移后退出（与「启动时自动迁移」并存，二选一口径见回执）
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/config"
	"github.com/chadhao/jx-lab-trace/internal/httpapi"
	"github.com/chadhao/jx-lab-trace/internal/store"
)

// version 由构建脚本注入（-ldflags "-X main.version=..."）。
var version = "dev"

func main() {
	migrateOnly := flag.Bool("migrate", false, "只执行数据库迁移后退出（不监听端口）")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	// .env 只补**未设置**的变量；真实环境变量优先（生产可不带 .env）。
	if err := config.LoadDotEnv(config.DotEnvPath()); err != nil {
		fatal("读取 .env 失败", err)
	}
	cfg, err := config.Load()
	if err != nil {
		fatal("配置校验失败，拒绝启动", err)
	}
	logf("jxlabtrace version=%s · 生效配置（密码类已脱敏）:", version)
	for _, line := range cfg.Describe() {
		logf("  %s", line)
	}

	st, err := store.Open(cfg.DBDSN)
	if err != nil {
		fatal("数据库连接失败", err)
	}
	defer st.Close()
	// 留样保留期限默认值走配置（JX_RETENTION_MONTHS_*，docs/01 D4 —— 可配置不硬编码）。
	st.SetRetentionDefaults(store.RetentionDefaults{
		RawMonths: cfg.RetentionMonthsRaw, IntermediateMonths: cfg.RetentionMonthsIntermediate,
		FGMonths: cfg.RetentionMonthsFG, ArbitrationMonths: cfg.RetentionMonthsArbitration,
	})

	mctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	res, err := st.Migrate(mctx, cfg.BootstrapSysAdminOpenID)
	cancel()
	if err != nil {
		fatal("迁移失败", err)
	}
	logf("迁移完成：新建表 %d · 已存在跳过 %d · 权限点 %d · 角色 %d · 新补授权 %d（库内共 %d 行）· 清理字典外权限点 %d · 引导系统管理员 %q",
		res.TablesCreated, res.TablesExisted, res.PointsUpserted, res.RolesUpserted,
		res.GrantsInserted, res.GrantsTotal, res.PointsRemoved, res.BootstrapAdmin)
	if n, err := st.TableCount(context.Background()); err == nil {
		logf("表数 = %d", n)
	}

	if *migrateOnly {
		logf("-migrate：迁移结束，退出")
		return
	}

	srv := httpapi.New(&cfg, st, version)
	e := srv.Handler()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logf("收到退出信号，关闭 HTTP 服务")
		shctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = e.Shutdown(shctx)
	}()

	logf("监听 http://%s （会话 TTL=%s · dev 模式=%v）", cfg.HTTPAddr, cfg.SessionTTL, cfg.DevMode)
	if err := e.Start(cfg.HTTPAddr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("HTTP 服务启动失败", err)
	}
	logf("已退出")
}

func logf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[%s] "+format+"\n", append([]interface{}{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "[%s] %s: %v\n", time.Now().Format("2006-01-02 15:04:05"), msg, err)
	os.Exit(1)
}
