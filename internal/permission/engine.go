package permission

// 授权级别（docs/01 §8.0：4 级 + 无）。
const (
	LevelAll     = "ALL"     // 全（增 / 改 / 停）
	LevelRead    = "READ"    // 只读
	LevelInit    = "INIT"    // 仅发起
	LevelApprove = "APPROVE" // 仅审批
	LevelNone    = "NONE"    // 无
)

// Levels 是某账号在**某一个权限点**上持有的级别集合：
// 多角色取并集（docs/01 §8.3）；无任何记录 ⇒ 空集合 ⇒ 视为 NONE（deny by default）。
type Levels []string

// Granted 判定「该点是否可用」：并集中存在任一非 NONE 级别。
func (ls Levels) Granted() bool {
	for _, l := range ls {
		if l != "" && l != LevelNone {
			return true
		}
	}
	return false
}

// Allows 判定「是否达到 required 级别」：
// required ∈ 并集，或并集中含 ALL（ALL 覆盖该点声明的全部级别）。
// 空集合（无记录）对任何 required 一律 false ⇒ 无记录即拒绝。
func (ls Levels) Allows(required string) bool {
	if required == "" {
		return false
	}
	for _, l := range ls {
		if l == required || l == LevelAll {
			return true
		}
	}
	return false
}

// Union 合并多组级别（多角色取并集）：结果去重，并**剔除 NONE**
// （NONE＝「该角色在此点无授权」，并集语义下等价于不存在这条记录）。
// ⇒ 归一化后的不变量：空集合 或 全部为非 NONE 级别。
func Union(groups ...Levels) Levels {
	seen := map[string]bool{}
	var out Levels
	for _, g := range groups {
		for _, l := range g {
			if l == "" || l == LevelNone || seen[l] {
				continue
			}
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}
