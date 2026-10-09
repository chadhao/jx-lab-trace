// 追踪码引擎的实现（生成 / 校验位 / 解析 / 人读行互转）。
//
// ★★ 唯一真源：spec/code-rules.json（status: frozen, v1）。
//	本文件里的常量与它逐条对齐，由 TestTC_M3_01_* 读取 ../../spec/code-rules.json
//	交叉核验（段序 / 段宽 / 字母表 / 校验算法 / 人读行分组 / 5 条示例向量）。
// ★ **纯函数**：不连库、不起服务即可断言（任务包 D1）。
//
// ★ 「格式正确但系统内不存在」不在这里判定 —— 本包只回答「是不是本系统的码、
//	段值是什么」；查库定位属 D2~D4 的定位环节。

package codec

import (
	"errors"
	"fmt"
	"strings"
)

// 常量（对齐 spec/code-rules.json#length / #alphabet / #segments）。
const (
	// Alphabet 是字符集：0-9 → 0-9，A-Z → 10-35。
	Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	// PayloadLen 是裸串长度（不含校验位）。
	PayloadLen = 26
	// TotalLen 是全码长度（裸串 + 校验位）。
	TotalLen = 27
	// Version 是当前编码版本位（V 段唯一已发布取值）。
	Version = '1'
)

// 解析 / 生成的错误分类 —— ★ 三种错误**必须可区分**：
//
//	ErrNotOurs   ：不是本系统的码（长度 / 首段非法）；
//	ErrChecksum  ：校验位不匹配（★ 校验位存在的全部意义）；
//	ErrBadInput  ：生成时的入参不合法 / 人读行分组不符（实现侧的格式错）。
var (
	ErrNotOurs  = errors.New("不是本系统的码")
	ErrChecksum = errors.New("码可能被读错，请重扫")
	ErrBadInput = errors.New("追踪码格式非法")
	ErrVersion  = errors.New("不支持的编码版本（当前仅实现 v1）")
)

// Segments 是 26 位裸串的段视图（spec#segments 的结构化表达）。
type Segments struct {
	V        string // 编码版本      1 位 [1-9]
	T        string // 对象类型      1 位 [ABCDE]
	BT       string // 业务类型      2 位 CG/ZG
	Customer string // 客户编号      4 位（ZG 填 0000）
	Material string // 物料编号      4 位
	Date     string // 链根日期 YYMMDD 6 位
	SEQ1     string // 序1（第 1 层） 2 位
	SEQ2     string // 序2（第 2 层） 3 位
	SEQ3     string // 序3（第 3 层） 3 位
}

// Parsed 是解析结果：裸串 + 全码 + 段视图 + 人读行。
type Parsed struct {
	Payload string
	Full    string
	Human   string
	Seg     Segments
}

// ObjectSpec 描述一种对象类型在码中的层级语义（spec#object_types）。
type ObjectSpec struct {
	Type         byte   // T 段取值
	Name         string // 中文名
	Chain        string // 原料链 / 成品链
	Depth        int    // 层级深度
	MaterialSlot string // 物料段的含义
	Seq1         string // 序1 含义
	Seq2         string // 序2 含义
	Seq3         string // 序3 含义
}

// objectTypes 对齐 spec/code-rules.json#object_types。
var objectTypes = map[byte]ObjectSpec{
	'A': {Type: 'A', Name: "车次", Chain: "原料链", Depth: 1, MaterialSlot: "原料物料", Seq1: "车序", Seq2: "000", Seq3: "000"},
	'B': {Type: 'B', Name: "原料吨袋", Chain: "原料链", Depth: 2, MaterialSlot: "原料物料", Seq1: "车序", Seq2: "袋序", Seq3: "000"},
	'C': {Type: 'C', Name: "生产批", Chain: "成品链", Depth: 1, MaterialSlot: "成品物料(计划产出)", Seq1: "批序", Seq2: "000", Seq3: "000"},
	'D': {Type: 'D', Name: "成品批", Chain: "成品链", Depth: 2, MaterialSlot: "成品物料", Seq1: "来源生产批序", Seq2: "该生产批内第n个成品批", Seq3: "000"},
	'E': {Type: 'E', Name: "成品吨袋", Chain: "成品链", Depth: 3, MaterialSlot: "成品物料", Seq1: "来源生产批序", Seq2: "成品批序", Seq3: "袋序"},
}

// ObjectOf 返回对象类型的层级语义；未知类型返回 false。
func ObjectOf(t byte) (ObjectSpec, bool) {
	spec, ok := objectTypes[t]
	return spec, ok
}

// Placeholder 是「本对象没有这一层」的占位值（spec#rules.other_side_placeholder）。
const Placeholder = "000"

// ===== 生成 =====

// Generate 按段序拼裸串 → 算第 27 位校验位 → 返回全码。
//
// ★ V 缺省填当前版本 '1'；ZG 的客户段由调用方填 0000（spec#segments.CUSTOMER）。
func Generate(seg Segments) (string, error) {
	if seg.V == "" {
		s := seg
		s.V = string(Version)
		seg = s
	}
	if err := seg.Validate(); err != nil {
		return "", err
	}
	payload := seg.V + seg.T + seg.BT + seg.Customer + seg.Material + seg.Date +
		seg.SEQ1 + seg.SEQ2 + seg.SEQ3
	if len(payload) != PayloadLen {
		return "", fmt.Errorf("%w：段拼接后应为 %d 位，实际 %d 位", ErrBadInput, PayloadLen, len(payload))
	}
	ch, err := CheckChar(payload)
	if err != nil {
		return "", err
	}
	return payload + string(ch), nil
}

// CheckChar 按 spec#check_algorithm 计算校验位：
// weighted_mod · modulus=36 · right_to_left · weights=[1,3] · 字符值取 alphabet 下标。
func CheckChar(payload string) (byte, error) {
	if len(payload) != PayloadLen {
		return 0, fmt.Errorf("%w：裸串应为 %d 位，实际 %d 位", ErrBadInput, PayloadLen, len(payload))
	}
	sum, err := WeightedSum(payload)
	if err != nil {
		return 0, err
	}
	return Alphabet[sum%len(Alphabet)], nil
}

// WeightedSum 返回校验位计算的加权和 S（供回归用例直接比对 weighted_sum）。
//
// ★ 从右向左：最右位权 1、次右位权 3，1/3 交替。
func WeightedSum(payload string) (int, error) {
	weights := [2]int{1, 3}
	total := 0
	for i, k := len(payload)-1, 0; i >= 0; i, k = i-1, k+1 {
		v, ok := charValue(payload[i])
		if !ok {
			return 0, fmt.Errorf("%w：字符 %q 不在字母表内", ErrBadInput, payload[i])
		}
		total += v * weights[k%2]
	}
	return total, nil
}

// ===== 解析 =====

// Parse 按 spec#parse_steps 逐步短路解析全码（27 位）。
//
//	长度 != 27        → ErrNotOurs
//	V 不在 [1-9]      → ErrNotOurs
//	T 不在 [ABCDE]    → ErrNotOurs
//	校验位不匹配       → ErrChecksum（★ 提示「请重扫」）
//	段值不合法         → ErrNotOurs
//	通过              → 结构化结果（★ 「系统内是否存在」由调用方查库判定）
func Parse(full string) (Parsed, error) {
	if len(full) != TotalLen {
		return Parsed{}, fmt.Errorf("%w：长度 %d ≠ %d", ErrNotOurs, len(full), TotalLen)
	}
	payload, check := full[:PayloadLen], full[PayloadLen]

	if full[0] < '1' || full[0] > '9' {
		return Parsed{}, fmt.Errorf("%w：版本位 %q 不在 [1-9]", ErrNotOurs, full[0])
	}
	if _, ok := objectTypes[full[1]]; !ok {
		return Parsed{}, fmt.Errorf("%w：对象类型 %q 不在 [ABCDE]", ErrNotOurs, full[1])
	}
	want, err := CheckChar(payload)
	if err != nil {
		return Parsed{}, err
	}
	if want != check {
		return Parsed{}, fmt.Errorf("%w：校验位应为 %c，实际 %c", ErrChecksum, want, check)
	}
	if full[0] != Version {
		return Parsed{}, fmt.Errorf("%w：%c", ErrVersion, full[0])
	}

	seg := Segments{
		V:        full[0:1],
		T:        full[1:2],
		BT:       full[2:4],
		Customer: full[4:8],
		Material: full[8:12],
		Date:     full[12:18],
		SEQ1:     full[18:20],
		SEQ2:     full[20:23],
		SEQ3:     full[23:26],
	}
	if err := seg.Validate(); err != nil {
		return Parsed{}, fmt.Errorf("%w：%v", ErrNotOurs, err)
	}
	human, err := ToHuman(full)
	if err != nil {
		return Parsed{}, err
	}
	return Parsed{Payload: payload, Full: full, Human: human, Seg: seg}, nil
}

// ===== 人读行 =====

// humanGroups 是人读行的分组宽度（spec#code_faces.human_groups）。
//
// ★ 与 segments 的分组【不同】：人读行把 V + T 并成 2 位「对象头」（1A / 1E）。
var humanGroups = [9]int{2, 2, 4, 4, 6, 2, 3, 3, 1}

// ToHuman 把全码转成带分隔符的人读行（严格同构、可无损互转）。
func ToHuman(full string) (string, error) {
	if len(full) != TotalLen {
		return "", fmt.Errorf("%w：长度 %d ≠ %d", ErrNotOurs, len(full), TotalLen)
	}
	parts := make([]string, 0, len(humanGroups))
	i := 0
	for _, w := range humanGroups {
		parts = append(parts, full[i:i+w])
		i += w
	}
	return strings.Join(parts, "-"), nil
}

// FromHuman 把人读行还原成 27 位裸串形态的全码（先校分组、再走完整解析）。
func FromHuman(human string) (string, error) {
	parts := strings.Split(human, "-")
	if len(parts) != len(humanGroups) {
		return "", fmt.Errorf("%w：人读行应有 %d 组，实际 %d 组", ErrNotOurs, len(humanGroups), len(parts))
	}
	var b strings.Builder
	for i, p := range parts {
		if len(p) != humanGroups[i] {
			return "", fmt.Errorf("%w：第 %d 组宽度应为 %d，实际 %d", ErrNotOurs, i+1, humanGroups[i], len(p))
		}
		b.WriteString(p)
	}
	if b.Len() != TotalLen {
		return "", fmt.Errorf("%w：拼回长度 %d ≠ %d", ErrNotOurs, b.Len(), TotalLen)
	}
	full := b.String()
	if _, err := Parse(full); err != nil {
		return "", err
	}
	return full, nil
}

// ===== 段校验 =====

// Validate 校验段视图的取值与**层级语义**（宽度 · 字符集 · 序段占位）。
//
// ★ 原料链（A/B）的 SEQ3 恒 000 —— 是「层级只有 2」的正常表达，不是未定义位。
func (s Segments) Validate() error {
	digits := func(v string, w int, name string) error {
		if len(v) != w {
			return fmt.Errorf("%s 应为 %d 位，实际 %q", name, w, v)
		}
		for i := 0; i < len(v); i++ {
			if v[i] < '0' || v[i] > '9' {
				return fmt.Errorf("%s 必须是数字，实际 %q", name, v)
			}
		}
		return nil
	}

	if len(s.V) != 1 || s.V[0] < '1' || s.V[0] > '9' {
		return fmt.Errorf("版本位 %q 不在 [1-9]", s.V)
	}
	if len(s.T) != 1 {
		return fmt.Errorf("对象类型 %q 不在 [ABCDE]", s.T)
	}
	spec, ok := objectTypes[s.T[0]]
	if !ok {
		return fmt.Errorf("对象类型 %q 不在 [ABCDE]", s.T)
	}
	if s.BT != "CG" && s.BT != "ZG" {
		return fmt.Errorf("业务类型 %q 不在 [CG,ZG]", s.BT)
	}
	if err := digits(s.Customer, 4, "客户段"); err != nil {
		return err
	}
	if s.BT == "ZG" && s.Customer != "0000" {
		return fmt.Errorf("自购料（ZG）客户段应填保留值 0000，实际 %q", s.Customer)
	}
	if err := digits(s.Material, 4, "物料段"); err != nil {
		return err
	}
	if err := digits(s.Date, 6, "日期段"); err != nil {
		return err
	}
	if err := digits(s.SEQ1, 2, "序1"); err != nil {
		return err
	}
	if err := digits(s.SEQ2, 3, "序2"); err != nil {
		return err
	}
	if err := digits(s.SEQ3, 3, "序3"); err != nil {
		return err
	}

	// 序段 = 从链根到本对象的路径：本层没有的段必须是占位 000（spec#object_types）。
	if spec.Seq2 == Placeholder && s.SEQ2 != Placeholder {
		return fmt.Errorf("%s 是第 1 层对象，序2 应为 000，实际 %q", spec.Name, s.SEQ2)
	}
	if spec.Seq3 == Placeholder && s.SEQ3 != Placeholder {
		return fmt.Errorf("%s 的层级深度为 %d，序3 恒为 000，实际 %q", spec.Name, spec.Depth, s.SEQ3)
	}
	return nil
}

// SeqPath 返回「从链根到本对象」的序段路径（不含占位段之前的层级）。
//
// 例：车次 A → ["03"]；原料吨袋 B → ["03","007"]；成品吨袋 E → ["03","002","015"]。
func (s Segments) SeqPath() []string {
	spec, ok := objectTypes[s.T[0]]
	if !ok {
		return nil
	}
	all := []string{s.SEQ1, s.SEQ2, s.SEQ3}
	return all[:spec.Depth]
}

// charValue 取字符在字母表中的值（0-9 → 0-9，A-Z → 10-35）。
func charValue(c byte) (int, bool) {
	if c >= '0' && c <= '9' {
		return int(c - '0'), true
	}
	if c >= 'A' && c <= 'Z' {
		return int(c-'A') + 10, true
	}
	return 0, false
}
