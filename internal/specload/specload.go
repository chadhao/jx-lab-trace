// Package specload 是 spec 判据双引擎的 **Go 侧**（N-023 D1.3）。
//
// ★ 双引擎：本包与 scripts/check_spec.py（Python 侧）各自独立实现
//
//	spec/checks.json#kinds 全部 kind 的求值器，对**同一份** checks.json 求值。
//	任一引擎不支持某 kind（新增 kind 未两侧同步）或任一判据不过 ⇒ 失败。
//
// ★ 消费方式：specload.Run 由本包 TestSpecChecks（go test ./... 必绿项）驱动；
//
//	Python 侧由 scripts/check_spec.py 进门禁。两引擎对 kinds 全集的一致性
//	由 checks.json#kinds 声明 + 双侧「未知 kind 即失败」共同钉住。
//
// 求值所需的 spec 文件从仓库根的 spec/ 读取（测试从包目录运行 ⇒ ../../spec）。
package specload

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ChecksDoc 对应 spec/checks.json。
type ChecksDoc struct {
	Spec    string   `json:"spec"`
	SpecVer int      `json:"spec_version"`
	Kinds   []string `json:"kinds"`
	Checks  []Check  `json:"checks"`
}

// Check 是单条判据（kind 决定用哪些字段）。
type Check struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Desc   string `json:"desc"`
	Expect int    `json:"expect"`

	Table   string   `json:"table"`
	Column  string   `json:"column"`
	Index   string   `json:"index"`
	Columns []string `json:"columns"`

	Letter string `json:"letter"`
	Name   string `json:"name"`

	Enum   string   `json:"enum"`
	Values []string `json:"values"`

	Form   string   `json:"form"`
	Fields []string `json:"fields"`

	File    string `json:"file"`
	Pattern string `json:"pattern"`

	Method string `json:"method"`
	Path   string `json:"path"`
}

// Result 是单条判据的求值结果。
type Result struct {
	ID     string
	Desc   string
	Passed bool
	Detail string
}

// schemaTable 是 schema.sql 中一张表的结构视图。
type schemaTable struct {
	Columns    map[string]bool
	UniqueKeys map[string][]string
	Indexes    map[string][]string
}

// Engine 持有一次求值所需的全部输入。
type Engine struct {
	Root      string // 仓库根
	Doc       *ChecksDoc
	Schema    map[string]*schemaTable
	CodeRules map[string]interface{}
	Enums     map[string]interface{}
	Forms     map[string]map[string]interface{}
}

// Load 从 repoRoot 读入 checks.json 与全部被引用的 spec 文件。
func Load(repoRoot string) (*Engine, error) {
	e := &Engine{Root: repoRoot}
	raw, err := os.ReadFile(filepath.Join(repoRoot, "spec", "checks.json"))
	if err != nil {
		return nil, fmt.Errorf("读取 spec/checks.json 失败: %w", err)
	}
	if err := json.Unmarshal(raw, &e.Doc); err != nil {
		return nil, fmt.Errorf("解析 spec/checks.json 失败: %w", err)
	}
	sqlText, err := os.ReadFile(filepath.Join(repoRoot, "spec", "schema.sql"))
	if err != nil {
		return nil, fmt.Errorf("读取 spec/schema.sql 失败: %w", err)
	}
	e.Schema = parseSchema(string(sqlText))

	if e.CodeRules, err = readJSONMap(filepath.Join(repoRoot, "spec", "code-rules.json")); err != nil {
		return nil, err
	}
	if e.Enums, err = readJSONMap(filepath.Join(repoRoot, "spec", "enums.json")); err != nil {
		return nil, err
	}
	formsDir := filepath.Join(repoRoot, "spec", "forms")
	ents, err := os.ReadDir(formsDir)
	if err != nil {
		return nil, fmt.Errorf("读取 spec/forms 失败: %w", err)
	}
	e.Forms = map[string]map[string]interface{}{}
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		f, err := readJSONMap(filepath.Join(formsDir, ent.Name()))
		if err != nil {
			return nil, err
		}
		name, _ := f["form"].(string)
		if name == "" {
			return nil, fmt.Errorf("spec/forms/%s 缺 form 字段", ent.Name())
		}
		e.Forms[name] = f
	}
	return e, nil
}

func readJSONMap(path string) (map[string]interface{}, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return m, nil
}

// parseSchema 解析 CREATE TABLE 语句，提取列 / 唯一键 / 索引。
// 与 Python 侧 check_spec.parse_schema 同一套规则（去注释行 → 逐行匹配）。
func parseSchema(text string) map[string]*schemaTable {
	tables := map[string]*schemaTable{}
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		lines = append(lines, line)
	}
	clean := strings.Join(lines, "\n")
	re := regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(\w+)\s*\((.*?)\)\s*(ENGINE|;)`)
	for _, m := range re.FindAllStringSubmatch(clean, -1) {
		name := strings.ToLower(m[1])
		t := &schemaTable{
			Columns:    map[string]bool{},
			UniqueKeys: map[string][]string{},
			Indexes:    map[string][]string{},
		}
		reU := regexp.MustCompile(`(?i)UNIQUE\s+KEY\s+(\w+)\s*\(([^)]*)\)`)
		reI := regexp.MustCompile(`(?i)KEY\s+(\w+)\s*\(([^)]*)\)`)
		reC := regexp.MustCompile("(?i)^(`?\\w+`?)\\s+[A-Z]")
		for _, raw := range strings.Split(m[2], "\n") {
			line := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), ","))
			if line == "" {
				continue
			}
			if mu := reU.FindStringSubmatch(line); mu != nil {
				t.UniqueKeys[strings.ToLower(mu[1])] = splitCols(mu[2])
				continue
			}
			if mi := reI.FindStringSubmatch(line); mi != nil {
				t.Indexes[strings.ToLower(mi[1])] = splitCols(mi[2])
				continue
			}
			if mc := reC.FindStringSubmatch(line); mc != nil {
				col := strings.ToLower(strings.Trim(mc[1], "`"))
				first := strings.ToUpper(strings.Fields(line)[0])
				if first != "PRIMARY" && first != "CONSTRAINT" && first != "FOREIGN" &&
					first != "CHECK" && first != "UNIQUE" && first != "KEY" &&
					first != "FULLTEXT" && first != "SPATIAL" && first != "INDEX" {
					t.Columns[col] = true
				}
			}
		}
		tables[name] = t
	}
	return tables
}

func splitCols(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.ToLower(strings.Trim(strings.TrimSpace(p), "`")))
	}
	return out
}

// Run 对 checks.json 全部判据求值，返回逐条结果。
// 未知 kind ⇒ 该条失败（与 Python 引擎同一约定：两侧必须同批支持新 kind）。
func (e *Engine) Run() []Result {
	declared := map[string]bool{}
	for _, k := range e.Doc.Kinds {
		declared[k] = true
	}
	out := make([]Result, 0, len(e.Doc.Checks))
	for _, c := range e.Doc.Checks {
		r := Result{ID: c.ID, Desc: c.Desc}
		if !declared[c.Kind] {
			r.Detail = fmt.Sprintf("判据使用了未声明的 kind %q（kinds 全集: %v）", c.Kind, e.Doc.Kinds)
			out = append(out, r)
			continue
		}
		if err := e.eval(c); err != nil {
			r.Detail = err.Error()
		} else {
			r.Passed = true
		}
		out = append(out, r)
	}
	return out
}

func (e *Engine) eval(c Check) error {
	switch c.Kind {
	case "table_count":
		if len(e.Schema) != c.Expect {
			return fmt.Errorf("表数 %d != 期望 %d", len(e.Schema), c.Expect)
		}
	case "schema_table_exists":
		if _, ok := e.Schema[strings.ToLower(c.Table)]; !ok {
			return fmt.Errorf("表 %s 不存在", c.Table)
		}
	case "schema_unique_key":
		t, ok := e.Schema[strings.ToLower(c.Table)]
		if !ok {
			return fmt.Errorf("表 %s 不存在", c.Table)
		}
		want := lowerAll(c.Columns)
		for _, v := range t.UniqueKeys {
			if strSliceEqual(v, want) {
				return nil
			}
		}
		return fmt.Errorf("唯一键 %v 不存在（现有 %v）", want, t.UniqueKeys)
	case "schema_column_present":
		t, ok := e.Schema[strings.ToLower(c.Table)]
		if !ok {
			return fmt.Errorf("表 %s 不存在", c.Table)
		}
		if !t.Columns[strings.ToLower(c.Column)] {
			return fmt.Errorf("列 %s 不存在", c.Column)
		}
	case "schema_column_absent":
		t, ok := e.Schema[strings.ToLower(c.Table)]
		if !ok {
			return fmt.Errorf("表 %s 不存在", c.Table)
		}
		if t.Columns[strings.ToLower(c.Column)] {
			return fmt.Errorf("列 %s 应已删除但仍在", c.Column)
		}
	case "schema_index_present":
		t, ok := e.Schema[strings.ToLower(c.Table)]
		if !ok {
			return fmt.Errorf("表 %s 不存在", c.Table)
		}
		got, ok := t.Indexes[strings.ToLower(c.Index)]
		if !ok {
			return fmt.Errorf("索引 %s 不存在（现有 %v）", c.Index, t.Indexes)
		}
		if len(c.Columns) > 0 && !strSliceEqual(got, lowerAll(c.Columns)) {
			return fmt.Errorf("索引 %s 列 %v != 期望 %v", c.Index, got, c.Columns)
		}
	case "schema_index_absent":
		t, ok := e.Schema[strings.ToLower(c.Table)]
		if !ok {
			return fmt.Errorf("表 %s 不存在", c.Table)
		}
		if _, ok := t.Indexes[strings.ToLower(c.Index)]; ok {
			return fmt.Errorf("索引 %s 应已移除但仍在", c.Index)
		}
	case "code_rules_object_type":
		ots, _ := e.CodeRules["object_types"].(map[string]interface{})
		ot, ok := ots[c.Letter]
		if !ok {
			return fmt.Errorf("对象类型 %s 未登记", c.Letter)
		}
		m, _ := ot.(map[string]interface{})
		if name, _ := m["name"].(string); name != c.Name {
			return fmt.Errorf("名称 %q != 期望 %q", name, c.Name)
		}
	case "enum_values":
		enums, _ := e.Enums["enums"].(map[string]interface{})
		raw, ok := enums[c.Enum]
		if !ok {
			return fmt.Errorf("枚举 %s 不存在", c.Enum)
		}
		m, _ := raw.(map[string]interface{})
		got := toStringSlice(m["values"])
		if !strSliceEqual(got, c.Values) {
			return fmt.Errorf("%v != 期望 %v", got, c.Values)
		}
	case "form_fields_present":
		f, ok := e.Forms[c.Form]
		if !ok {
			return fmt.Errorf("表单 %s 不存在", c.Form)
		}
		have := formFields(f)
		for _, k := range c.Fields {
			if !have[k] {
				return fmt.Errorf("缺字段 %s", k)
			}
		}
	case "form_fields_absent":
		f, ok := e.Forms[c.Form]
		if !ok {
			return fmt.Errorf("表单 %s 不存在", c.Form)
		}
		have := formFields(f)
		for _, k := range c.Fields {
			if have[k] {
				return fmt.Errorf("不应存在的字段 %s", k)
			}
		}
	case "source_regex":
		s, err := e.readFile(c.File)
		if err != nil {
			return err
		}
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return fmt.Errorf("正则非法 %q: %w", c.Pattern, err)
		}
		if !re.MatchString(s) {
			return fmt.Errorf("正则未命中: %s", c.Pattern)
		}
	case "source_absent_regex":
		s, err := e.readFile(c.File)
		if err != nil {
			return err
		}
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return fmt.Errorf("正则非法 %q: %w", c.Pattern, err)
		}
		if re.MatchString(s) {
			return fmt.Errorf("正则命中（应不存在）: %s", c.Pattern)
		}
	case "route_registered":
		s, err := e.readFile(filepath.Join("internal", "httpapi", "recv.go"))
		if err != nil {
			return err
		}
		rel := c.Path
		if strings.HasPrefix(rel, "/api/recv") {
			rel = strings.TrimPrefix(rel, "/api/recv")
		}
		pat := regexp.MustCompile(`g\.` + regexp.QuoteMeta(c.Method) + `\("` + regexp.QuoteMeta(rel) + `"`)
		if !pat.MatchString(s) {
			return fmt.Errorf("路由 %s %s 未在 recv.go 注册", c.Method, c.Path)
		}
	default:
		return fmt.Errorf("未知 kind %q —— 双引擎需同批支持", c.Kind)
	}
	return nil
}

func (e *Engine) readFile(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(e.Root, rel))
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败: %w", rel, err)
	}
	return string(b), nil
}

func formFields(f map[string]interface{}) map[string]bool {
	out := map[string]bool{}
	fields, _ := f["fields"].([]interface{})
	for _, raw := range fields {
		m, _ := raw.(map[string]interface{})
		if k, _ := m["key"].(string); k != "" {
			out[k] = true
		}
	}
	return out
}

func toStringSlice(v interface{}) []string {
	arr, _ := v.([]interface{})
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		out = append(out, fmt.Sprintf("%v", x))
	}
	return out
}

func lowerAll(s []string) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = strings.ToLower(x)
	}
	return out
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// IDs 返回判据 id 列表（排序，供跨引擎对账）。
func (d *ChecksDoc) IDs() []string {
	out := make([]string, 0, len(d.Checks))
	for _, c := range d.Checks {
		out = append(out, c.ID)
	}
	sort.Strings(out)
	return out
}

// ReadFile 供测试读仓库文件（导出小工具，避免测试里重复路径拼接）。
func ReadFile(root, rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
