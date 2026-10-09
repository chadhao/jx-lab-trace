package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TC 配置口径（任务包 D2）：一律有安全缺省，或明确拒绝启动。

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"JX_DB_DSN", "JX_DEV_MODE", "JX_DEV_OPEN_ID", "JX_FEISHU_APP_ID",
		"JX_FEISHU_APP_SECRET", "JX_SESSION_TTL", "JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID",
		"JX_HTTP_ADDR", "JX_ATTACH_DIR", "JX_ATTACH_MAX_MB",
		"JX_RETENTION_MONTHS_RAW", "JX_RETENTION_MONTHS_INTERMEDIATE",
		"JX_RETENTION_MONTHS_FG", "JX_RETENTION_MONTHS_ARBITRATION",
	} {
		t.Setenv(k, "")
	}
}

func TestConfig_DSNRequired_RefusesToStart(t *testing.T) {
	clearEnv(t)
	_, err := Load()
	if err == nil {
		t.Fatal("★ 缺 JX_DB_DSN 必须拒绝启动（该变量无缺省）")
	}
	if !strings.Contains(err.Error(), "JX_DB_DSN") {
		t.Fatalf("错误信息应点名 JX_DB_DSN，实际: %v", err)
	}
}

func TestConfig_DevModeRequiresOpenID(t *testing.T) {
	clearEnv(t)
	t.Setenv("JX_DB_DSN", "u:p@tcp(127.0.0.1:3306)/db")
	t.Setenv("JX_DEV_MODE", "true")
	if _, err := Load(); err == nil {
		t.Fatal("dev 模式缺 JX_DEV_OPEN_ID 必须拒绝启动")
	}
	t.Setenv("JX_DEV_OPEN_ID", "ou_x")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("补齐后应可启动: %v", err)
	}
	if !cfg.DevMode || cfg.DevOpenID != "ou_x" {
		t.Fatalf("配置装配错误: %+v", cfg)
	}
}

func TestConfig_DefaultsAreSafe(t *testing.T) {
	clearEnv(t)
	t.Setenv("JX_DB_DSN", "u:p@tcp(127.0.0.1:3306)/db")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("最小配置应可启动: %v", err)
	}
	if cfg.SessionTTL != 12*time.Hour {
		t.Fatalf("JX_SESSION_TTL 缺省应为 12h，实际 %s", cfg.SessionTTL)
	}
	if cfg.HTTPAddr != "127.0.0.1:8080" {
		t.Fatalf("JX_HTTP_ADDR 缺省应为 127.0.0.1:8080，实际 %s", cfg.HTTPAddr)
	}
	if cfg.AttachDir != "/srv/jx-lab-trace/attachments" {
		t.Fatalf("JX_ATTACH_DIR 缺省错误: %s", cfg.AttachDir)
	}
	if cfg.DevMode {
		t.Fatal("JX_DEV_MODE 缺省必须 false（真实模式）")
	}
	if !cfg.SecureCookie {
		t.Fatal("非 dev 模式 cookie 必须 Secure")
	}
	if cfg.CookieName != "jx_sid" {
		t.Fatalf("cookie 名错误: %s", cfg.CookieName)
	}
}

func TestConfig_InvalidValuesRefused(t *testing.T) {
	clearEnv(t)
	t.Setenv("JX_DB_DSN", "u:p@tcp(127.0.0.1:3306)/db")

	t.Setenv("JX_SESSION_TTL", "banana")
	if _, err := Load(); err == nil {
		t.Fatal("非法 TTL 必须拒绝启动")
	}
	t.Setenv("JX_SESSION_TTL", "")

	t.Setenv("JX_DEV_MODE", "maybe")
	if _, err := Load(); err == nil {
		t.Fatal("非法 JX_DEV_MODE 必须拒绝启动")
	}
	t.Setenv("JX_DEV_MODE", "")

	t.Setenv("JX_FEISHU_APP_ID", "cli_x")
	if _, err := Load(); err == nil {
		t.Fatal("飞书 AppID/Secret 必须成对提供")
	}
}

func TestConfig_RedactDSN_NoPlaintextPassword(t *testing.T) {
	clearEnv(t)
	t.Setenv("JX_DB_DSN", "jx_lab:SuperSecret@tcp(10.0.0.1:3306)/jx_lab_trace")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	joined := strings.Join(cfg.Describe(), "\n")
	if strings.Contains(joined, "SuperSecret") {
		t.Fatalf("★ 启动回显不得出现明文口令:\n%s", joined)
	}
	if !strings.Contains(joined, "jx_lab:***@tcp(10.0.0.1:3306)") {
		t.Fatalf("回显应给出脱敏后的 DSN:\n%s", joined)
	}
}

func TestConfig_LoadDotEnv_DoesNotOverrideRealEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "JX_DB_DSN=from_file\n# comment\nJX_HTTP_ADDR=127.0.0.1:9999\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写 .env 失败: %v", err)
	}

	t.Setenv("JX_DB_DSN", "from_process")
	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv 失败: %v", err)
	}
	if got := os.Getenv("JX_DB_DSN"); got != "from_process" {
		t.Fatalf("★ .env 不得覆盖真实环境变量（实际 %q）", got)
	}
	if got := os.Getenv("JX_HTTP_ADDR"); got != "127.0.0.1:9999" {
		t.Fatalf(".env 应补入未设置的变量，实际 %q", got)
	}

	// 文件不存在 ⇒ 静默通过（生产不带 .env）
	if err := LoadDotEnv(filepath.Join(dir, "nope.env")); err != nil {
		t.Fatalf(".env 缺失不应报错: %v", err)
	}
}

// 留样保留期限默认月数可配置（docs/01 D4 / 任务包 §6-9：不得硬编码）。
func TestConfig_RetentionMonthsConfigurable(t *testing.T) {
	clearEnv(t)
	t.Setenv("JX_DB_DSN", "u:p@tcp(127.0.0.1:3306)/db")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("最小配置应可启动: %v", err)
	}
	if cfg.RetentionMonthsRaw != 6 || cfg.RetentionMonthsIntermediate != 3 ||
		cfg.RetentionMonthsFG != 12 || cfg.RetentionMonthsArbitration != 24 {
		t.Fatalf("保留月数缺省应 6/3/12/24，实际 %d/%d/%d/%d",
			cfg.RetentionMonthsRaw, cfg.RetentionMonthsIntermediate,
			cfg.RetentionMonthsFG, cfg.RetentionMonthsArbitration)
	}

	t.Setenv("JX_RETENTION_MONTHS_RAW", "3")
	t.Setenv("JX_RETENTION_MONTHS_ARBITRATION", "36")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("配置保留月数后应可启动: %v", err)
	}
	if cfg.RetentionMonthsRaw != 3 || cfg.RetentionMonthsArbitration != 36 {
		t.Fatalf("环境变量未生效: 原料=%d 仲裁=%d", cfg.RetentionMonthsRaw, cfg.RetentionMonthsArbitration)
	}

	t.Setenv("JX_RETENTION_MONTHS_FG", "banana")
	if _, err := Load(); err == nil {
		t.Fatal("非法保留月数必须拒绝启动")
	}
	t.Setenv("JX_RETENTION_MONTHS_FG", "0")
	if _, err := Load(); err == nil {
		t.Fatal("保留月数为 0 必须拒绝启动")
	}
}

// 附件单文件上限可配置（批 5 D4 / §6-16：不得硬编码；非法即拒绝启动）。
func TestConfig_AttachMaxMBConfigurable(t *testing.T) {
	clearEnv(t)
	t.Setenv("JX_DB_DSN", "u:p@tcp(127.0.0.1:3306)/db")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("最小配置应可启动: %v", err)
	}
	if cfg.AttachMaxMB != 20 {
		t.Fatalf("JX_ATTACH_MAX_MB 缺省应为 20，实际 %d", cfg.AttachMaxMB)
	}

	t.Setenv("JX_ATTACH_MAX_MB", "50")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("配置附件上限后应可启动: %v", err)
	}
	if cfg.AttachMaxMB != 50 {
		t.Fatalf("环境变量未生效: 实际 %d", cfg.AttachMaxMB)
	}

	t.Setenv("JX_ATTACH_MAX_MB", "banana")
	if _, err := Load(); err == nil {
		t.Fatal("非法附件上限必须拒绝启动")
	}
	t.Setenv("JX_ATTACH_MAX_MB", "0")
	if _, err := Load(); err == nil {
		t.Fatal("附件上限为 0 必须拒绝启动")
	}
}
