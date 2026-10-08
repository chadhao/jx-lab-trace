package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
)

// ===== 判定限解析（M1 / D3）=====
//
// ★ 判定限按「客户 × 物料」维度配置，**不是**一张全局表，也**不内联**在
//	m_test_item 上（docs/01 §5.1 / 附录 A：内联等于默认「所有客户同一标准」）。
// ★ 解析顺序（取**第一条命中**，最具体者优先）：
//	(test_item, customer, material) → (test_item, customer, 0)
//	→ (test_item, 0, material)     → (test_item, 0, 0)
// ★ `customer_id = 0` 或 `material_id = 0` 表示通用默认。
//
// ★ 设计要点：**优先级判定是纯函数**（ResolveLimit / LimitRank），
//	不需要起服务、不需要连库即可断言顺序 ⇒ TC-M1-05 / TC-M1-06 有本地单测。

// LimitRow 是 m_test_item_limit 的一行（解析结果）。
// ★ Lower/Upper 为 nil 表示该端无限制（SQL NULL）—— 不用空串，
//
//	空的 json.Number 在序列化时会报「invalid number literal」。
type LimitRow struct {
	ID         int64       `json:"id"`
	TestItemID int64       `json:"test_item_id"`
	CustomerID int64       `json:"customer_id"`
	MaterialID int64       `json:"material_id"`
	Lower      interface{} `json:"lower_limit"`
	Upper      interface{} `json:"upper_limit"`
	IsRequired bool        `json:"is_required"`
	Note       string      `json:"note"`
	Status     string      `json:"status"`
	Version    int64       `json:"version"`
	Rank       int         `json:"rank"` // 解析时填：越大约具体
}

// Rank 返回候选 scope 对 (wantCustomer, wantMaterial) 的**具体度**：
//
//	3 = (want, want) 精确命中
//	2 = (want, 0)    客户专属
//	1 = (0, want)    物料专属
//	0 = (0, 0)       通用默认
//	-1 = 与本次查询无关（既不是命中维度，也不是 0）
//
// ★ 与 D3 的顺序一致：客户维度排在物料维度之前（先 (c,0) 后 (0,m)）。
func LimitRank(customerID, materialID, wantCustomer, wantMaterial int64) int {
	switch {
	case customerID == wantCustomer && materialID == wantMaterial:
		return 3
	case customerID == wantCustomer && materialID == 0:
		return 2
	case customerID == 0 && materialID == wantMaterial:
		return 1
	case customerID == 0 && materialID == 0:
		return 0
	default:
		return -1
	}
}

// ResolveLimit 从候选集里挑出**最具体**的一条（同 rank 时取 version 大、id 大者）。
// 返回 (命中行, true) 或 (零值, false)。
//
// ★ 纯函数：不查库、不依赖服务 ⇒ 可直接单测优先级顺序（TC-M1-05 / TC-M1-06）。
func ResolveLimit(candidates []LimitRow, wantCustomer, wantMaterial int64) (LimitRow, bool) {
	best := LimitRow{}
	bestRank := -1
	found := false
	for _, c := range candidates {
		if c.Status != "" && c.Status != "启用" {
			continue
		}
		r := LimitRank(c.CustomerID, c.MaterialID, wantCustomer, wantMaterial)
		if r < 0 {
			continue
		}
		switch {
		case !found,
			r > bestRank,
			r == bestRank && c.Version > best.Version,
			r == bestRank && c.Version == best.Version && c.ID > best.ID:
			best, bestRank, found = c, r, true
		}
	}
	return best, found
}

// LookupLimit 查某检测项在给定「客户 × 物料」下的生效判定限。
//
// 一次取回 4 个可能 scope 的当前启用行，再交给纯函数定序（保证单测与线上同一条判定路径）。
func (s *Store) LookupLimit(ctx context.Context, testItemID, customerID, materialID int64) (LimitRow, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, test_item_id, customer_id, material_id,
		       IFNULL(lower_limit, ''), IFNULL(upper_limit, ''),
		       is_required, IFNULL(note, ''), status, version, supersedes_id
		  FROM m_test_item_limit
		 WHERE test_item_id = ? AND is_current = 1 AND status = '启用'
		   AND customer_id IN (?, 0) AND material_id IN (?, 0)`,
		testItemID, customerID, materialID)
	if err != nil {
		return LimitRow{}, false, fmt.Errorf("查询判定限失败: %w", err)
	}
	defer rows.Close()

	var cands []LimitRow
	for rows.Next() {
		var r LimitRow
		var lower, upper string
		var sup sql.NullInt64
		var req int
		if err := rows.Scan(&r.ID, &r.TestItemID, &r.CustomerID, &r.MaterialID,
			&lower, &upper, &req, &r.Note, &r.Status, &r.Version, &sup); err != nil {
			return LimitRow{}, false, fmt.Errorf("读取判定限失败: %w", err)
		}
		r.Lower = nullIfEmptyNum(lower)
		r.Upper = nullIfEmptyNum(upper)
		r.IsRequired = req == 1
		cands = append(cands, r)
	}
	if err := rows.Err(); err != nil {
		return LimitRow{}, false, err
	}
	hit, ok := ResolveLimit(cands, customerID, materialID)
	if ok {
		hit.Rank = LimitRank(hit.CustomerID, hit.MaterialID, customerID, materialID)
	}
	return hit, ok, nil
}

// nullIfEmptyNum 把 DECIMAL 的定长尾零去掉；空串（=SQL NULL）⇒ nil。
func nullIfEmptyNum(s string) interface{} {
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return json.Number(s)
	}
	if f == float64(int64(f)) {
		return json.Number(fmt.Sprintf("%d", int64(f)))
	}
	return json.Number(strconv.FormatFloat(f, 'f', -1, 64))
}
