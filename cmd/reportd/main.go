// Command reportd：报告快照的**公网侧静态服务进程**（批 8 · D4 / D7）。
//
// ★★★ 零数据库连接（docs/03 批 8 验收要点第 ④ 条 / §6-2 / A10）：
//
//	本文件只用标准库，**不 import internal/store、不 import internal/config、
//	不读 JX_DB_DSN、不链接任何 SQL 驱动**。有效性判定不查库 ——
//	「文件在不在 served/」就是有效性：撤销 / 过期由内网侧把快照移入
//	_inactive/ ⇒ 这里自然 404。
//
// ★ 路由：GET /r/<token>.html ⇒ 命中 served/ 则 200（text/html），否则 404；
//
//	GET /healthz ⇒ ok（部署 smoke 用）。
//
// ★ 每次访问报告（不含 /healthz）追加一行 TSV 到 JX_REPORT_DIR/access.log：
//
//	<RFC3339Nano>\t<token>\t<remote_ip>\t<status>\t<user_agent>
//	★ token / IP / UA 里的制表符与换行必须转义（防日志注入）。
//
// ★ 只绑回环（缺省 127.0.0.1:18090，与主服务同口径；非回环地址拒绝启动）。
// ★ 追加写用 O_APPEND 且**常驻 fd**：内网同步走 byte offset（不 rename 日志），
//
//	两侧不会互相丢行（§6-8）。
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	addrFlag := flag.String("addr", "", "监听地址（缺省取 JX_REPORTD_ADDR，再缺省 127.0.0.1:18090）")
	flag.Parse()

	addr := strings.TrimSpace(*addrFlag)
	if addr == "" {
		addr = strings.TrimSpace(os.Getenv("JX_REPORTD_ADDR"))
	}
	if addr == "" {
		addr = "127.0.0.1:18090"
	}
	if err := checkLoopback(addr); err != nil {
		fmt.Fprintln(os.Stderr, "reportd: "+err.Error())
		os.Exit(1)
	}

	root := reportDir()
	served := filepath.Join(root, "served")
	if err := os.MkdirAll(served, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "reportd: 无法创建快照目录:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "reportd: 无法创建报告根目录:", err)
		os.Exit(1)
	}

	logFile, err := os.OpenFile(filepath.Join(root, "access.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reportd: 无法打开访问日志:", err)
		os.Exit(1)
	}
	defer logFile.Close()

	srv := &http.Server{
		Addr:    addr,
		Handler: newHandler(served, logFile),
		// 读超时收紧：静态页不需要长连接
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Printf("reportd: 目录=%s 监听=%s（零 DB 连接）\n", served, addr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "reportd:", err)
		os.Exit(1)
	}
}

// reportDir 快照根目录：JX_REPORT_DIR，缺省 ~/jx-lab-trace/reports
// （★ 与 internal/store/report.go 的缺省保持同一形态；这里刻意不共享代码 ——
// reportd 一旦 import internal/store 就会连上数据库）。
func reportDir() string {
	if v := strings.TrimSpace(os.Getenv("JX_REPORT_DIR")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "reports"
	}
	return filepath.Join(home, "jx-lab-trace", "reports")
}

// checkLoopback 拒绝非回环监听（§D7：不得绑 0.0.0.0，与主服务同口径）。
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("监听地址 %q 非法: %v", addr, err)
	}
	if host == "" {
		return fmt.Errorf("监听地址 %q 必须显式给出回环 host（如 127.0.0.1）", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		return fmt.Errorf("监听地址 %q 不是回环地址，拒绝启动", addr)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("监听地址 %q 不是回环地址，拒绝启动", addr)
	}
	return nil
}

// tokenFileRe 只接受 base64url 字符集的 <token>.html（防路径穿越）。
func validSnapshotName(name string) bool {
	if !strings.HasSuffix(name, ".html") || strings.Contains(name, "/") ||
		strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return false
	}
	base := strings.TrimSuffix(name, ".html")
	if len(base) < 32 || base == "" {
		return false
	}
	for _, r := range base {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// statusWriter 记录响应状态码（访问日志要写真实 status）。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// logAccess 追加一行 TSV（★ 字段内制表符 / 换行转义 ⇒ 防日志注入）。
func logAccess(f *os.File, token, ip string, status int, ua string) {
	line := strings.Join([]string{
		time.Now().Format(time.RFC3339Nano),
		sanitize(token),
		sanitize(ip),
		fmt.Sprintf("%d", status),
		sanitize(ua),
	}, "\t")
	_, _ = f.WriteString(line + "\n")
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func newHandler(served string, logFile *os.File) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/r/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/r/")
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		token := name
		if strings.HasSuffix(token, ".html") {
			token = strings.TrimSuffix(token, ".html")
		}
		if !validSnapshotName(name) {
			http.NotFound(sw, r)
			logAccess(logFile, token, clientIP(r), sw.status, r.Header.Get("User-Agent"))
			return
		}
		f, err := os.Open(filepath.Join(served, name))
		if err != nil {
			// ★ 文件不在 served/ ⇒ 404（撤销 / 过期的自然结果，不查库）
			http.NotFound(sw, r)
			logAccess(logFile, token, clientIP(r), sw.status, r.Header.Get("User-Agent"))
			return
		}
		defer f.Close()
		st, serr := f.Stat()
		if serr != nil || st.IsDir() {
			http.NotFound(sw, r)
			logAccess(logFile, token, clientIP(r), sw.status, r.Header.Get("User-Agent"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(sw, r, name, st.ModTime(), f)
		logAccess(logFile, token, clientIP(r), sw.status, r.Header.Get("User-Agent"))
	})

	// 其它路径一律 404（不提供目录列表、不透出文件系统）
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return mux
}

// clientIP 取直接来源 IP（一期无反代；与同步去重键一致）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
