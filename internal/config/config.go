// Package config 从环境变量装配运行配置。
//
// ★ 口径（任务包 D2）：**一律有安全缺省或明确拒绝启动**；
//
//	改配置后在启动日志回显生效值，**密码类不打明文**。
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 缺省值。JX_DB_DSN **故意没有缺省** —— 缺失即拒绝启动。
const (
	defaultSessionTTL = 12 * time.Hour
	defaultHTTPAddr   = "127.0.0.1:8080"
	defaultAttachDir  = "/srv/jx-lab-trace/attachments"
	defaultCookieName = "jx_sid"
	dotEnvPath        = ".env"
	errDevOpenID      = "JX_DEV_MODE=true 时必须显式给 JX_DEV_OPEN_ID（dev 桩的固定身份），否则拒绝启动"
)

// Config 是运行期配置（已校验、可直接使用）。
type Config struct {
	DBDSN                   string        // JX_DB_DSN：无缺省，缺失即拒绝启动
	DevMode                 bool          // JX_DEV_MODE：开发模式（免登桩），缺省 false
	DevOpenID               string        // JX_DEV_OPEN_ID：dev 模式固定身份
	FeishuAppID             string        // JX_FEISHU_APP_ID：真实飞书免登
	FeishuAppSecret         string        // JX_FEISHU_APP_SECRET
	SessionTTL              time.Duration // JX_SESSION_TTL：缺省 12h
	BootstrapSysAdminOpenID string        // JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID：首个系统管理员引导
	HTTPAddr                string        // JX_HTTP_ADDR：缺省 127.0.0.1:8080
	AttachDir               string        // JX_ATTACH_DIR：缺省 /srv/jx-lab-trace/attachments
	AttachMaxMB             int           // JX_ATTACH_MAX_MB：附件单文件上限（MB），缺省 20（任务包 D4）
	CookieName              string        // 会话 cookie 名（固定，不走环境变量）
	SecureCookie            bool          // 生产加 Secure（非 dev 模式即视为生产）

	// 留样保留期限默认月数（docs/01 D4；★ 可配置不硬编码，任务包 §6-9）：
	// JX_RETENTION_MONTHS_RAW / _INTERMEDIATE / _FG / _ARBITRATION，缺省 6 / 3 / 12 / 24。
	RetentionMonthsRaw          int
	RetentionMonthsIntermediate int
	RetentionMonthsFG           int
	RetentionMonthsArbitration  int
}

// Load 读取并校验环境变量。任何不合法配置 ⇒ 返回 error（调用方必须拒绝启动）。
func Load() (Config, error) {
	var c Config

	c.DBDSN = strings.TrimSpace(os.Getenv("JX_DB_DSN"))
	if c.DBDSN == "" {
		return c, fmt.Errorf("缺少 JX_DB_DSN：该变量无缺省，必须显式提供（形如 user:pass@tcp(host:port)/db?charset=utf8mb4&parseTime=true&loc=Local）")
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("JX_DEV_MODE"))) {
	case "", "false", "0", "no", "off":
		c.DevMode = false
	case "true", "1", "yes", "on":
		c.DevMode = true
	default:
		return c, fmt.Errorf("JX_DEV_MODE 取值非法：%q（只接受 true/false）", os.Getenv("JX_DEV_MODE"))
	}

	c.DevOpenID = strings.TrimSpace(os.Getenv("JX_DEV_OPEN_ID"))
	if c.DevMode && c.DevOpenID == "" {
		return c, fmt.Errorf("%s", errDevOpenID)
	}

	c.FeishuAppID = strings.TrimSpace(os.Getenv("JX_FEISHU_APP_ID"))
	c.FeishuAppSecret = strings.TrimSpace(os.Getenv("JX_FEISHU_APP_SECRET"))
	if (c.FeishuAppID == "") != (c.FeishuAppSecret == "") {
		return c, fmt.Errorf("JX_FEISHU_APP_ID 与 JX_FEISHU_APP_SECRET 必须成对提供")
	}

	c.SessionTTL = defaultSessionTTL
	if v := strings.TrimSpace(os.Getenv("JX_SESSION_TTL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("JX_SESSION_TTL 取值非法：%q（须为正的 Go duration，如 12h / 30m）", v)
		}
		c.SessionTTL = d
	}

	c.BootstrapSysAdminOpenID = strings.TrimSpace(os.Getenv("JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID"))

	c.HTTPAddr = strings.TrimSpace(os.Getenv("JX_HTTP_ADDR"))
	if c.HTTPAddr == "" {
		c.HTTPAddr = defaultHTTPAddr
	}

	c.AttachDir = strings.TrimSpace(os.Getenv("JX_ATTACH_DIR"))
	if c.AttachDir == "" {
		c.AttachDir = defaultAttachDir
	}

	// 附件单文件上限（MB）：缺省 20；非法 ⇒ 明确拒绝启动（与 monthsFromEnv 同一手法）。
	var err error
	if c.AttachMaxMB, err = mbFromEnv("JX_ATTACH_MAX_MB", 20); err != nil {
		return c, err
	}

	c.CookieName = defaultCookieName
	c.SecureCookie = !c.DevMode

	// 留样保留期限默认月数（D4：原料 6 月 / 中间 3 月 / 成品 12 月 / 仲裁 24 月）。
	if c.RetentionMonthsRaw, err = monthsFromEnv("JX_RETENTION_MONTHS_RAW", 6); err != nil {
		return c, err
	}
	if c.RetentionMonthsIntermediate, err = monthsFromEnv("JX_RETENTION_MONTHS_INTERMEDIATE", 3); err != nil {
		return c, err
	}
	if c.RetentionMonthsFG, err = monthsFromEnv("JX_RETENTION_MONTHS_FG", 12); err != nil {
		return c, err
	}
	if c.RetentionMonthsArbitration, err = monthsFromEnv("JX_RETENTION_MONTHS_ARBITRATION", 24); err != nil {
		return c, err
	}
	return c, nil
}

// monthsFromEnv 读取「保留月数」环境变量：未设 ⇒ 缺省；非正整数 ⇒ 明确拒绝启动。
func monthsFromEnv(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 1200 {
		return 0, fmt.Errorf("%s 取值非法：%q（须为 1~1200 的整数月）", key, v)
	}
	return n, nil
}

// mbFromEnv 读取「MB 上限」环境变量（JX_ATTACH_MAX_MB）：未设 ⇒ 缺省；
// 非正整数或超上限 ⇒ 明确拒绝启动。
func mbFromEnv(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 10240 {
		return 0, fmt.Errorf("%s 取值非法：%q（须为 1~10240 的整数 MB）", key, v)
	}
	return n, nil
}

var dsnSecretRe = regexp.MustCompile(`(?s)^([^:@/]+):[^@]*@`)

// RedactDSN 把 DSN 里的口令打码，用于启动日志回显。
func RedactDSN(dsn string) string {
	if dsn == "" {
		return "(空)"
	}
	return dsnSecretRe.ReplaceAllString(dsn, "$1:***@")
}

// Describe 返回「生效值回显」行（密码类已脱敏）。
func (c Config) Describe() []string {
	mode := "real"
	if c.DevMode {
		mode = "dev(免登桩)"
	}
	feishu := "未配置(仅 dev 桩可用)"
	if c.FeishuAppID != "" {
		feishu = "已配置(app_id=" + c.FeishuAppID + ", secret=***)"
	}
	return []string{
		"JX_DB_DSN          = " + RedactDSN(c.DBDSN),
		"JX_DEV_MODE        = " + fmt.Sprintf("%v", c.DevMode),
		"JX_DEV_OPEN_ID     = " + orDash(c.DevOpenID),
		"JX_FEISHU          = " + feishu,
		"JX_SESSION_TTL     = " + c.SessionTTL.String(),
		"JX_BOOTSTRAP_ADMIN = " + orDash(c.BootstrapSysAdminOpenID),
		"JX_HTTP_ADDR       = " + c.HTTPAddr,
		"JX_ATTACH_DIR      = " + c.AttachDir,
		fmt.Sprintf("attach max MB      = %d", c.AttachMaxMB),
		fmt.Sprintf("retention months   = 原料%d/中间%d/成品%d/仲裁%d",
			c.RetentionMonthsRaw, c.RetentionMonthsIntermediate,
			c.RetentionMonthsFG, c.RetentionMonthsArbitration),
		"auth mode          = " + mode,
		"cookie             = name=" + c.CookieName + " HttpOnly SameSite=Lax Secure=" + fmt.Sprintf("%v", c.SecureCookie),
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// LoadDotEnv 把 path 指向的 .env 文件里**尚未设置**的变量注入进程环境。
// ★ 真实环境变量优先（文件不覆盖）；文件不存在 ⇒ 静默返回（生产不带 .env）。
func LoadDotEnv(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	return nil
}

// DotEnvPath 返回工作目录下 .env 的路径（供 main 与测试共用口径）。
func DotEnvPath() string { return dotEnvPath }
