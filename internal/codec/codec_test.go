package codec

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ===== 码引擎 · 回归基线 =====
//
// ★★ 回归基线 = spec/code-rules.json 的 5 条 sample_vectors（已由
//	spec/verify_code_rules.py 自校验过，不是草稿）。本文件同时做两件事：
//	① 逐条比对 weighted_sum / remainder / check / full / human（TC-M3-01）；
//	② 把实现里的常量与 spec 逐字段交叉核验（防止实现与规格各说各话）。

type specFile struct {
	SpecVersion int    `json:"spec_version"`
	Status      string `json:"status"`
	Alphabet    string `json:"alphabet"`
	Length      struct {
		Payload int `json:"payload"`
		Check   int `json:"check"`
		Total   int `json:"total"`
	} `json:"length"`
	CheckAlgorithm struct {
		Algorithm string `json:"algorithm"`
		Modulus   int    `json:"modulus"`
		Direction string `json:"direction"`
		Weights   []int  `json:"weights"`
	} `json:"check_algorithm"`
	Segments []struct {
		Key   string `json:"key"`
		Name  string `json:"name"`
		Width int    `json:"width"`
	} `json:"segments"`
	ObjectTypes map[string]struct {
		Name  string `json:"name"`
		Chain string `json:"chain"`
		Depth int    `json:"depth"`
		Seq1  string `json:"seq1"`
		Seq2  string `json:"seq2"`
		Seq3  string `json:"seq3"`
	} `json:"object_types"`
	Rules struct {
		OtherSidePlaceholder string `json:"other_side_placeholder"`
	} `json:"rules"`
	CodeFaces struct {
		HumanGroups []int `json:"human_groups"`
	} `json:"code_faces"`
	SampleVectors []struct {
		Object      string `json:"object"`
		Payload     string `json:"payload"`
		WeightedSum int    `json:"weighted_sum"`
		Remainder   int    `json:"remainder"`
		Check       string `json:"check"`
		Full        string `json:"full"`
		Human       string `json:"human"`
	} `json:"sample_vectors"`
}

func loadSpec(t *testing.T) *specFile {
	t.Helper()
	path := filepath.Join("..", "..", "spec", "code-rules.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	var spec specFile
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("解析 spec/code-rules.json 失败: %v", err)
	}
	return &spec
}

// segsOf 把 26 位裸串按 spec 的段序切成分段视图。
func segsOf(t *testing.T, payload string) Segments {
	t.Helper()
	if len(payload) != PayloadLen {
		t.Fatalf("裸串应 %d 位，实际 %d 位：%s", PayloadLen, len(payload), payload)
	}
	// 段序：V T BT CUSTOMER MATERIAL DATE SEQ1 SEQ2 SEQ3 = 1 1 2 4 4 6 2 3 3
	return Segments{
		V: payload[0:1], T: payload[1:2], BT: payload[2:4],
		Customer: payload[4:8], Material: payload[8:12], Date: payload[12:18],
		SEQ1: payload[18:20], SEQ2: payload[20:23], SEQ3: payload[23:26],
	}
}

// TC-M3-01 ★★ 向量回归：5 条示例向量逐条生成，校验位三字段（加权和 /
// 余数 / 校验字符）与 full 全部一致；并做解析回读与人读行比对。
func TestTC_M3_01_VectorRegression(t *testing.T) {
	spec := loadSpec(t)
	if len(spec.SampleVectors) != 5 {
		t.Fatalf("示例向量应为 5 条，实际 %d 条", len(spec.SampleVectors))
	}
	for _, v := range spec.SampleVectors {
		v := v
		t.Run(v.Full, func(t *testing.T) {
			// ① 加权和 / 余数 / 校验字符 三字段逐个比对（A2）
			sum, err := WeightedSum(v.Payload)
			if err != nil {
				t.Fatalf("加权和计算失败: %v", err)
			}
			if sum != v.WeightedSum {
				t.Fatalf("weighted_sum 应 %d，实际 %d", v.WeightedSum, sum)
			}
			if rem := sum % len(Alphabet); rem != v.Remainder {
				t.Fatalf("remainder 应 %d，实际 %d", v.Remainder, rem)
			}
			ch, err := CheckChar(v.Payload)
			if err != nil {
				t.Fatalf("校验位计算失败: %v", err)
			}
			if string(ch) != v.Check {
				t.Fatalf("check 应 %q，实际 %q", v.Check, ch)
			}

			// ② 按段重新生成 ⇒ 必须等于 full（生成方向）
			got, err := Generate(segsOf(t, v.Payload))
			if err != nil {
				t.Fatalf("生成失败: %v", err)
			}
			if got != v.Full {
				t.Fatalf("生成的全码应 %s，实际 %s", v.Full, got)
			}

			// ③ 解析方向：段值与裸串一致、full / human 对得上
			p, err := Parse(v.Full)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if p.Payload != v.Payload {
				t.Fatalf("解析出的裸串应 %s，实际 %s", v.Payload, p.Payload)
			}
			if p.Full != v.Full {
				t.Fatalf("解析出的全码应 %s，实际 %s", v.Full, p.Full)
			}
			if p.Human != v.Human {
				t.Fatalf("人读行应 %s，实际 %s", v.Human, p.Human)
			}
			if want := segsOf(t, v.Payload); p.Seg != want {
				t.Fatalf("段视图不一致：%+v vs %+v", p.Seg, want)
			}
		})
	}
}

// A5 / TC-M3-01 附加：5 条向量的 human ↔ full 双向转换往返一致（严格同构）。
func TestTC_M3_01_HumanRoundTrip(t *testing.T) {
	spec := loadSpec(t)
	for _, v := range spec.SampleVectors {
		h, err := ToHuman(v.Full)
		if err != nil {
			t.Fatalf("ToHuman(%s) 失败: %v", v.Full, err)
		}
		if h != v.Human {
			t.Fatalf("ToHuman 应 %s，实际 %s", v.Human, h)
		}
		back, err := FromHuman(h)
		if err != nil {
			t.Fatalf("FromHuman(%s) 失败: %v", h, err)
		}
		if back != v.Full {
			t.Fatalf("往返不一致：%s → %s", v.Full, back)
		}
	}
}

// TC-M3-02 ★ 变异：改动车码任意一位（取字母表相邻字符，保证加权和必变）⇒
// 解析**因校验位不匹配**被拒（提示「请重扫」）。
func TestTC_M3_02_ChecksumMismatchRejected(t *testing.T) {
	spec := loadSpec(t)
	full := spec.SampleVectors[0].Full // 车次码
	if full[1:2] != "A" {
		t.Fatalf("第 1 条向量应是车次码，实际 %s", full)
	}

	for i := 0; i < TotalLen; i++ {
		orig := full[i]
		// 取字母表中相邻字符：Δ值 = ±1 ⇒ 加权和必变（±1 或 ±3，mod 36 ≠ 0）
		pos := strings.IndexByte(Alphabet, orig)
		if pos < 0 {
			t.Fatalf("字符 %q 不在字母表内", orig)
		}
		next := Alphabet[(pos+1)%len(Alphabet)]
		mutated := full[:i] + string(next) + full[i+1:]

		_, err := Parse(mutated)
		if err == nil {
			t.Fatalf("第 %d 位由 %c 改为 %c 后，解析应失败", i+1, orig, next)
		}
		// 首两段（V / T）先被 parse_steps 短路，属预期；其余一律是校验位拒绝
		if i == 0 || i == 1 {
			if !errors.Is(err, ErrNotOurs) && !errors.Is(err, ErrChecksum) {
				t.Fatalf("第 %d 位变异应被拒绝，实际错误: %v", i+1, err)
			}
			continue
		}
		if !errors.Is(err, ErrChecksum) {
			t.Fatalf("第 %d 位变异应报「码可能被读错，请重扫」，实际: %v", i+1, err)
		}
		if !strings.Contains(err.Error(), "请重扫") {
			t.Fatalf("第 %d 位变异的提示须含「请重扫」，实际: %v", i+1, err)
		}
	}
}

// TC-M3-03 异常：别系统的码 / 长度不对 / 首段非法 ⇒ 拒绝，提示「不是本系统的码」。
func TestTC_M3_03_NotOurCode(t *testing.T) {
	spec := loadSpec(t)
	full := spec.SampleVectors[0].Full

	cases := []struct {
		name string
		code string
	}{
		{"长度少一位", full[:TotalLen-1]},
		{"长度多一位", full + "0"},
		{"空串", ""},
		{"版本位非法(0)", "0" + full[1:]},
		{"版本位非法(X)", "X" + full[1:]},
		{"对象类型非法(Z)", full[:1] + "Z" + full[2:]},
		{"对象类型非法(小写a)", full[:1] + "a" + full[2:]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.code)
			if err == nil {
				t.Fatalf("应当被拒绝：%q", c.code)
			}
			if !errors.Is(err, ErrNotOurs) {
				t.Fatalf("应报「不是本系统的码」，实际: %v", err)
			}
			if !strings.Contains(err.Error(), "不是本系统的码") {
				t.Fatalf("提示须含「不是本系统的码」，实际: %v", err)
			}
		})
	}
}

// 实现与机读规格的交叉核验（段序 / 段宽 / 字母表 / 校验算法 / 人读行 / 规则）。
func TestCodec_SpecCrossCheck(t *testing.T) {
	spec := loadSpec(t)

	if spec.Status != "frozen" {
		t.Fatalf("spec 状态应为 frozen，实际 %q", spec.Status)
	}
	if spec.Alphabet != Alphabet {
		t.Fatalf("字母表不一致：spec=%q 实现=%q", spec.Alphabet, Alphabet)
	}
	if spec.Length.Payload != PayloadLen || spec.Length.Total != TotalLen ||
		spec.Length.Check != 1 {
		t.Fatalf("长度定义不一致：spec=%+v 实现 payload=%d total=%d",
			spec.Length, PayloadLen, TotalLen)
	}
	if spec.CheckAlgorithm.Algorithm != "weighted_mod" ||
		spec.CheckAlgorithm.Modulus != len(Alphabet) ||
		spec.CheckAlgorithm.Direction != "right_to_left" {
		t.Fatalf("校验算法定义不一致：spec=%+v", spec.CheckAlgorithm)
	}
	if len(spec.CheckAlgorithm.Weights) != 2 ||
		spec.CheckAlgorithm.Weights[0] != 1 || spec.CheckAlgorithm.Weights[1] != 3 {
		t.Fatalf("权值应为 [1,3]，spec=%v", spec.CheckAlgorithm.Weights)
	}

	// 段序与段宽必须与 Generate 的拼接顺序一致
	wantOrder := []string{"V", "T", "BT", "CUSTOMER", "MATERIAL", "DATE", "SEQ1", "SEQ2", "SEQ3"}
	wantWidth := []int{1, 1, 2, 4, 4, 6, 2, 3, 3}
	if len(spec.Segments) != len(wantOrder) {
		t.Fatalf("段数应 %d，spec 实际 %d", len(wantOrder), len(spec.Segments))
	}
	total := 0
	for i, s := range spec.Segments {
		if s.Key != wantOrder[i] {
			t.Fatalf("第 %d 段应为 %s，spec 实际 %s", i+1, wantOrder[i], s.Key)
		}
		if s.Width != wantWidth[i] {
			t.Fatalf("段 %s 宽度应 %d，spec 实际 %d", s.Key, wantWidth[i], s.Width)
		}
		total += s.Width
	}
	if total != PayloadLen {
		t.Fatalf("段宽之和应 %d，实际 %d", PayloadLen, total)
	}

	// 人读行分组（★ 与 segments 分组不同：V+T 并成 2 位对象头）
	if len(spec.CodeFaces.HumanGroups) != len(humanGroups) {
		t.Fatalf("人读行组数不一致：spec=%v 实现=%v", spec.CodeFaces.HumanGroups, humanGroups)
	}
	sum := 0
	for i, w := range spec.CodeFaces.HumanGroups {
		if w != humanGroups[i] {
			t.Fatalf("人读行第 %d 组宽度应 %d，实现=%d", i+1, w, humanGroups[i])
		}
		sum += w
	}
	if sum != TotalLen {
		t.Fatalf("人读行组宽之和应 %d，实际 %d", TotalLen, sum)
	}

	// 对象类型与层级语义
	if len(spec.ObjectTypes) != len(objectTypes) {
		t.Fatalf("对象类型数不一致：spec=%d 实现=%d", len(spec.ObjectTypes), len(objectTypes))
	}
	for k, want := range spec.ObjectTypes {
		got, ok := objectTypes[k[0]]
		if !ok {
			t.Fatalf("实现缺少对象类型 %s", k)
		}
		if got.Name != want.Name || got.Chain != want.Chain ||
			got.Depth != want.Depth || got.Seq1 != want.Seq1 ||
			got.Seq2 != want.Seq2 || got.Seq3 != want.Seq3 {
			t.Fatalf("对象类型 %s 语义不一致：spec=%+v 实现=%+v", k, want, got)
		}
	}
	if spec.Rules.OtherSidePlaceholder != Placeholder {
		t.Fatalf("占位值应 %q，spec=%q", Placeholder, spec.Rules.OtherSidePlaceholder)
	}
}

// 序段 = 从链根到本对象的路径；★ 原料链（A/B）的 SEQ3 恒 000（不是未定义位）。
func TestCodec_SeqPathAndPlaceholder(t *testing.T) {
	spec := loadSpec(t)

	// A（车次）：只有 1 层路径，SEQ2/SEQ3 必须 000
	a := Segments{V: "1", T: "A", BT: "CG", Customer: "0001", Material: "0002",
		Date: "261007", SEQ1: "03", SEQ2: Placeholder, SEQ3: Placeholder}
	if _, err := Generate(a); err != nil {
		t.Fatalf("合法车次段应生成成功: %v", err)
	}
	if p := a.SeqPath(); len(p) != 1 || p[0] != "03" {
		t.Fatalf("车次的序段路径应 [03]，实际 %v", p)
	}
	bad := a
	bad.SEQ3 = "001"
	if _, err := Generate(bad); err == nil {
		t.Fatal("车次的序3 非 000 应被拒绝")
	}
	bad = a
	bad.SEQ2 = "001"
	if _, err := Generate(bad); err == nil {
		t.Fatal("车次的序2 非 000 应被拒绝")
	}

	// B（原料吨袋）：2 层路径，SEQ3 恒 000
	b := Segments{V: "1", T: "B", BT: "CG", Customer: "0001", Material: "0002",
		Date: "261007", SEQ1: "03", SEQ2: "007", SEQ3: Placeholder}
	if _, err := Generate(b); err != nil {
		t.Fatalf("合法吨袋段应生成成功: %v", err)
	}
	if p := b.SeqPath(); len(p) != 2 || p[1] != "007" {
		t.Fatalf("吨袋的序段路径应 [03 007]，实际 %v", p)
	}
	bad = b
	bad.SEQ3 = "001"
	if _, err := Generate(bad); err == nil {
		t.Fatal("原料链序3 非 000 应被拒绝")
	}

	// E（成品吨袋）：3 层路径全部展开
	e := Segments{V: "1", T: "E", BT: "CG", Customer: "0001", Material: "0003",
		Date: "261007", SEQ1: "03", SEQ2: "002", SEQ3: "015"}
	if _, err := Generate(e); err != nil {
		t.Fatalf("合法成品袋段应生成成功: %v", err)
	}
	if p := e.SeqPath(); len(p) != 3 {
		t.Fatalf("成品袋的序段路径应 3 层，实际 %v", p)
	}

	// 自购料（ZG）客户段必须是保留值 0000
	zg := Segments{V: "1", T: "A", BT: "ZG", Customer: "0001", Material: "0002",
		Date: "261007", SEQ1: "01", SEQ2: Placeholder, SEQ3: Placeholder}
	if _, err := Generate(zg); err == nil {
		t.Fatal("ZG 客户段非 0000 应被拒绝")
	}
	zg.Customer = "0000"
	if _, err := Generate(zg); err != nil {
		t.Fatalf("ZG 客户段 0000 应通过: %v", err)
	}

	// 与规格同一条断言的镜像（保证本测试的期望不是我临时编的）
	if spec.Rules.OtherSidePlaceholder != Placeholder {
		t.Fatalf("占位值不一致：%q", spec.Rules.OtherSidePlaceholder)
	}
}
