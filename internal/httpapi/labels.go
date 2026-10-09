package httpapi

// ===== D5 · 标签版式页与二维码 =====
//
// ★ spec#label.print_path：**服务端生成标签版式页 → 浏览器打印**（一期不写打印驱动）。
//	二维码内容 = 裸串 27 位、无分隔符（spec#code_faces.qrcode_content），
//	版式页同时印出人读行（spec#code_faces.human_line）。
// ★ 二维码用纯 Go 的 skip2/go-qrcode 生成（无 cgo、无传递依赖）——
//	自写 Reed-Solomon / 掩模选择的风险远高于引这个小依赖，理由见批 3 回执。

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/chadhao/jx-lab-trace/internal/codec"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
	qrcode "github.com/skip2/go-qrcode"
)

// handleLabelQR 输出单个码的二维码（SVG，矢量，打印不糊）。
//
// ★ 只接受**本系统的合法码**（先过 codec.Parse）—— 本入口不是通用二维码服务。
func (s *Server) handleLabelQR(c echo.Context) error {
	data := strings.ToUpper(strings.TrimSpace(c.QueryParam("data")))
	if _, err := codec.Parse(data); err != nil {
		return recvErr(err)
	}
	svg, err := qrSVG(data)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "生成二维码失败")
	}
	return c.Blob(http.StatusOK, "image/svg+xml; charset=utf-8", []byte(svg))
}

// handleLabelPage 渲染标签版式页（批量，供浏览器直接打印）。
func (s *Server) handleLabelPage(c echo.Context) error {
	raw := strings.TrimSpace(c.QueryParam("ids"))
	if raw == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "缺少 ids")
	}
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "ids 必须是逗号分隔的正整数")
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "ids 为空")
	}
	items, err := s.Store.GetLabelItems(c.Request().Context(), ids)
	if err != nil {
		return recvErr(err)
	}
	if len(items) == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "打印记录不存在")
	}
	autoprint := c.QueryParam("autoprint") == "1"
	return c.Blob(http.StatusOK, "text/html; charset=utf-8",
		[]byte(renderLabelPage(items, autoprint)))
}

// qrSVG 把内容渲染成 SVG 二维码（含静区；横向连续段合并成一条 path）。
func qrSVG(content string) (string, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return "", err
	}
	bm := q.Bitmap()
	n := len(bm)
	if n == 0 {
		return "", fmt.Errorf("二维码位图为空")
	}
	var d strings.Builder
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if !bm[y][x] {
				continue
			}
			x2 := x
			for x2+1 < n && bm[y][x2+1] {
				x2++
			}
			w := x2 - x + 1
			fmt.Fprintf(&d, "M%d %dh%dv1h-%dz", x, y, w, w)
			x = x2
		}
	}
	return fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="二维码">`+
			`<rect width="%d" height="%d" fill="#ffffff"/><path d="%s" fill="#000000"/></svg>`,
		n, n, n, n, d.String()), nil
}

// renderLabelPage 生成整页标签 HTML（每张：二维码 + 人读行 + 关键段位）。
func renderLabelPage(items []store.LabelItem, autoprint bool) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<title>标签打印</title><style>
:root { color-scheme: light; }
* { box-sizing: border-box; }
body { margin: 0; padding: 8mm; font: 12px/1.4 "Microsoft YaHei", sans-serif; background: #f4f4f4; }
.bar { margin-bottom: 6mm; }
.bar button { font-size: 14px; padding: 6px 16px; cursor: pointer; }
.labels { display: flex; flex-wrap: wrap; gap: 4mm; }
.label { width: 60mm; min-height: 40mm; background: #fff; border: 1px dashed #999;
         padding: 3mm; display: flex; flex-direction: column; align-items: center; gap: 1.5mm;
         break-inside: avoid; page-break-inside: avoid; }
.label .kind { font-size: 11px; color: #444; }
.label img, .label svg { width: 24mm; height: 24mm; }
.label .human { font: 10px/1.2 "Consolas", "Courier New", monospace; letter-spacing: .3px;
                word-break: break-all; text-align: center; }
.label .seg { font-size: 9px; color: #666; text-align: center; }
.label .zero { color: #bbb; }
@media print {
  body { background: #fff; padding: 0; }
  .bar { display: none; }
  .label { border: none; }
  @page { margin: 4mm; }
}
</style></head><body>`)
	b.WriteString(`<div class="bar"><button onclick="window.print()">打印本页</button> ` +
		`<span>共 ` + strconv.Itoa(len(items)) + ` 张</span></div>`)
	b.WriteString(`<div class="labels">`)
	for _, it := range items {
		b.WriteString(renderLabel(it))
	}
	b.WriteString(`</div>`)
	if autoprint {
		b.WriteString(`<script>window.print();</script>`)
	}
	b.WriteString(`</body></html>`)
	return b.String()
}

func renderLabel(it store.LabelItem) string {
	code := it.Code
	human := it.Human
	if human == "" {
		if h, err := codec.ToHuman(code); err == nil {
			human = h
		}
	}
	kind := it.Kind
	var segs codec.Segments
	var path []string
	if p, err := codec.Parse(code); err == nil {
		segs = p.Seg
		path = p.Seg.SeqPath()
		if kind == "" {
			if spec, ok := codec.ObjectOf(p.Seg.T[0]); ok {
				kind = spec.Name
			}
		}
	}
	var b strings.Builder
	b.WriteString(`<div class="label">`)
	b.WriteString(`<div class="kind">` + html.EscapeString(kind) + `</div>`)
	fmt.Fprintf(&b, `<img alt="二维码" src="/api/recv/labels/qr?data=%s">`, code)
	fmt.Fprintf(&b, `<div class="human">%s</div>`, html.EscapeString(human))
	fmt.Fprintf(&b, `<div class="seg">客户 %s · 物料 %s · %s · 序 %s</div>`,
		html.EscapeString(segs.Customer), html.EscapeString(segs.Material),
		html.EscapeString(dateCN(segs.Date)), html.EscapeString(strings.Join(path, "-")))
	if it.IsReprint == 1 {
		b.WriteString(`<div class="seg">补打 · ` + html.EscapeString(it.Reason) + `</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func dateCN(yymmdd string) string {
	if len(yymmdd) != 6 {
		return yymmdd
	}
	return "20" + yymmdd[0:2] + "-" + yymmdd[2:4] + "-" + yymmdd[4:6]
}
