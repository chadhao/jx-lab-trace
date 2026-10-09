package store

// ===== M9 报告分享 · 快照白名单投影与渲染（D1 / §6-3）=====
//
// ★★★ 快照内容受**白名单**约束：只有本文件的 snapshot* 结构体里的字段
//	允许出现在对外页面里；渲染层**只读这些结构**，禁止直接序列化 M8 的
//	BatchArchive / TraceFeed 等含内部字段的读模型（A7 / §6-3 实现建议）。
// ★★ 黑名单（三条，渲染后整体复核 guardSnapshot）：
//	① 「让步」/CONCESSION 字样及其变体（D20：对外不披露；内部档案照旧标注）；
//	② 其它客户的数据（生产批天然单客户，页内只渲染该批档案）；
//	③ remark / 操作人 / 作废留痕 / 紧急放行 / 审计 / 留样 / 班组人员等内部字段。

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"
)

// ===== 白名单投影结构（★ 类型即白名单） =====

// snapshotDoc 是一页对外报告快照的全部内容。
type snapshotDoc struct {
	Title         string
	ReportNo      string
	GeneratedAt   string
	ExpiresAt     string
	Customer      string
	InputMaterial string
	PlannedOutput string
	BatchHuman    string
	BatchDate     string
	Feeds         []snapshotFeed
	Operations    []snapshotOp
	FgLots        []snapshotFgLot
	Inspections   []snapshotInspRow
	Shipments     []snapshotShip
}

// snapshotFeed 投料明细：吨袋人读行 · 投料量 · 投料时间（白名单）。
type snapshotFeed struct {
	BagHuman   string
	FeedWeight string
	FedAt      string
}

// snapshotOp 作业段：段序 · 班组 · 时段 · 本段产出（白名单，★ 不含 operator）。
type snapshotOp struct {
	Seq    int
	Team   string
	Period string
	Output string
}

// snapshotFgLot 成品批：人读行 · 产出物料 · 净重（白名单）。
type snapshotFgLot struct {
	Human          string
	OutputMaterial string
	NetWeight      string
	Bags           []snapshotBag
}

// snapshotBag 成品袋：人读行 · 净重 · 状态（白名单）。
type snapshotBag struct {
	Human     string
	NetWeight string
	Status    string
}

// snapshotInspRow 检测结果：项目名 · 数值 · 单位 · 判定（白名单，仅此四列）。
type snapshotInspRow struct {
	Item  string
	Value string
	Unit  string
	Judge string
}

// snapshotShip 出货单：单号 · 状态 · 出场时间 · 车牌 · 客户（白名单）。
type snapshotShip struct {
	No       string
	Status   string
	ShipAt   string
	PlateNo  string
	Customer string
}

// ===== 格式化小工具（只产出展示串，不引入新字段） =====

func snapTime(p *time.Time) string {
	if p == nil || p.IsZero() {
		return "-"
	}
	return p.Local().Format("2006-01-02 15:04")
}

func snapTons(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.3f", *p)
}

// snapValue 检测数值：数值列优先，其次文本列。
func snapValue(num *float64, text string) string {
	if num != nil {
		return fmt.Sprintf("%g", *num)
	}
	if strings.TrimSpace(text) != "" {
		return text
	}
	return "-"
}

// ===== 渲染 =====

var snapshotTmpl = template.Must(template.New("snapshot").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · {{.ReportNo}}</title>
<style>
 body{font-family:-apple-system,"Segoe UI","Microsoft YaHei",sans-serif;margin:0;background:#f5f6f8;color:#1f2329}
 .wrap{max-width:960px;margin:0 auto;padding:24px}
 .card{background:#fff;border-radius:8px;padding:20px 24px;margin-bottom:16px;box-shadow:0 1px 2px rgba(0,0,0,.06)}
 h1{font-size:22px;margin:0 0 4px}
 h2{font-size:16px;margin:0 0 12px;border-left:4px solid #2b6de5;padding-left:8px}
 .meta{color:#646a73;font-size:13px;line-height:1.8}
 table{width:100%;border-collapse:collapse;font-size:13px}
 th,td{border:1px solid #e5e6eb;padding:6px 8px;text-align:left;word-break:break-all}
 th{background:#f2f3f5;font-weight:600}
 .num{text-align:right;font-variant-numeric:tabular-nums}
 .lot{margin-bottom:12px}
 .hint{color:#8a9099;font-size:12px}
</style>
</head>
<body>
<div class="wrap">
 <div class="card">
  <h1>{{.Title}}</h1>
  <div class="meta">
   报告编号：{{.ReportNo}}<br>
   生成时间：{{.GeneratedAt}}　有效期至：{{.ExpiresAt}}<br>
   客户：{{.Customer}}<br>
   输入物料：{{.InputMaterial}}　计划产出物料：{{.PlannedOutput}}<br>
   生产批：{{.BatchHuman}}　生产日期：{{.BatchDate}}
  </div>
 </div>

 <div class="card">
  <h2>投料明细</h2>
  {{if .Feeds}}
  <table><thead><tr><th>吨袋</th><th class="num">投料量（吨）</th><th>投料时间</th></tr></thead>
  <tbody>{{range .Feeds}}<tr><td>{{.BagHuman}}</td><td class="num">{{.FeedWeight}}</td><td>{{.FedAt}}</td></tr>{{end}}</tbody></table>
  {{else}}<div class="hint">无投料记录</div>{{end}}
 </div>

 <div class="card">
  <h2>作业段</h2>
  {{if .Operations}}
  <table><thead><tr><th>段序</th><th>班组</th><th>时段</th><th class="num">本段产出（吨）</th></tr></thead>
  <tbody>{{range .Operations}}<tr><td>{{.Seq}}</td><td>{{.Team}}</td><td>{{.Period}}</td><td class="num">{{.Output}}</td></tr>{{end}}</tbody></table>
  {{else}}<div class="hint">无作业段记录</div>{{end}}
 </div>

 <div class="card">
  <h2>成品</h2>
  {{range .FgLots}}
  <div class="lot">
   <table><thead><tr><th>成品批</th><th>产出物料</th><th class="num">净重（吨）</th></tr></thead>
   <tbody><tr><td>{{.Human}}</td><td>{{.OutputMaterial}}</td><td class="num">{{.NetWeight}}</td></tr></tbody></table>
   {{if .Bags}}
   <table style="margin-top:6px"><thead><tr><th>成品袋</th><th class="num">净重（吨）</th><th>状态</th></tr></thead>
   <tbody>{{range .Bags}}<tr><td>{{.Human}}</td><td class="num">{{.NetWeight}}</td><td>{{.Status}}</td></tr>{{end}}</tbody></table>
   {{end}}
  </div>
  {{else}}<div class="hint">无成品记录</div>{{end}}
 </div>

 <div class="card">
  <h2>检测结果</h2>
  {{if .Inspections}}
  <table><thead><tr><th>检测项目</th><th class="num">数值</th><th>单位</th><th>判定</th></tr></thead>
  <tbody>{{range .Inspections}}<tr><td>{{.Item}}</td><td class="num">{{.Value}}</td><td>{{.Unit}}</td><td>{{.Judge}}</td></tr>{{end}}</tbody></table>
  {{else}}<div class="hint">无检测结果</div>{{end}}
 </div>

 <div class="card">
  <h2>出货单</h2>
  {{if .Shipments}}
  <table><thead><tr><th>单号</th><th>状态</th><th>出场时间</th><th>车牌</th><th>客户</th></tr></thead>
  <tbody>{{range .Shipments}}<tr><td>{{.No}}</td><td>{{.Status}}</td><td>{{.ShipAt}}</td><td>{{.PlateNo}}</td><td>{{.Customer}}</td></tr>{{end}}</tbody></table>
  {{else}}<div class="hint">无出货记录</div>{{end}}
 </div>

 <div class="card hint">本页为静态快照，内容以生成时刻为准。</div>
</div>
</body>
</html>
`))

// snapshotForbiddenWords 是对外禁用词（黑名单第 ① 条，D20 定案）。
// ★ 小写比对 ⇒ 「CONCESSION」「concession」一并命中。
var snapshotForbiddenWords = []string{"让步", "concession"}

// renderSnapshot 渲染白名单投影（html/template 自动转义）。
func renderSnapshot(d *snapshotDoc) ([]byte, error) {
	var buf bytes.Buffer
	if err := snapshotTmpl.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("渲染报告快照失败: %w", err)
	}
	return buf.Bytes(), nil
}

// guardSnapshot 对**成品 HTML** 整体复核黑名单（类型白名单之外的第二道闸）。
// ★ 数据里带出禁用词时宁可失败，也不产出页面（A8 最高优先）。
func guardSnapshot(html []byte) error {
	low := strings.ToLower(string(html))
	for _, w := range snapshotForbiddenWords {
		if strings.Contains(low, w) {
			return fmt.Errorf("%w：命中对外禁用词 %q", ErrReportForbidden, w)
		}
	}
	return nil
}
