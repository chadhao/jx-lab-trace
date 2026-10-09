package store

// ===== M3 收货与打码 · 库侧 TC =====
//
// ★ 这些是 DB 集成测试：按 docs/05，本机未设 JX_TEST_DB=1 一律 Skip，
//	在测试服务器上以 JX_TEST_DB=1 执行（scripts/run_tc_server.sh）。
// ★ 固定的测试主数据（客户 9301 / 物料 9301）与固定链根日期，保证**可重复执行**：
//	每条用例开头先清掉上一轮残留，否则车序会从上次的号继续往后走。

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

const (
	m3StoreOpenID = "ou_test_m3_store"
	m3CustCode    = "9301"
	m3MatCode     = "9301"
	m3Date        = "2026-10-09" // 固定链根日期（车序空间的到货日）
)

// m3Wipe 清掉 M3 库侧测试数据。
//
// ★ 顺序硬约束：**先袋、后车**（袋码 uk_bag_code 全局唯一）——
//
//	只删车不删袋会留下「孤儿袋码」，下一轮生成同码袋必然 1062 撞号。
//
// ★ 范围硬约束：只删**本包账号**（ou_test_m3_store）造的行，
//
//	不得用 `LIKE 'ou_test_m3%'` 这类前缀 —— 那会连 httpapi 包（ou_test_m3_http）
//	的行一起删掉、且不删它的袋（已实测踩坑）。
func m3Wipe(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	me := m3StoreOpenID

	// ① 作废记录 → ② 投料/取样外键无（本包不造）→ ③ 袋 → ④ 车 → ⑤ 预报 → ⑥ 标签
	_, _ = st.DB().ExecContext(ctx,
		`DELETE FROM b_obj_void WHERE entity = 'b_bag'
		   AND entity_id IN (SELECT id FROM b_bag WHERE created_by = ?)`, me)
	_, _ = st.DB().ExecContext(ctx, `DELETE FROM b_bag WHERE created_by = ?`, me)
	_, _ = st.DB().ExecContext(ctx, `DELETE FROM b_truck_lot WHERE created_by = ?`, me)
	_, _ = st.DB().ExecContext(ctx, `DELETE FROM b_arrival_notice WHERE created_by = ?`, me)
	_, _ = st.DB().ExecContext(ctx, `DELETE FROM b_label_print WHERE created_by = ?`, me)

	// 主数据（按测试编号 + 测试账号前缀兜底）
	_, _ = st.DB().ExecContext(ctx,
		`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m3CustCode)
	_, _ = st.DB().ExecContext(ctx,
		`DELETE FROM m_material WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m3MatCode)
}

// m3Fixture 清场 + 建测试主数据（客户 9301 / 原料物料 9301），返回其 id。
func m3Fixture(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	st := migrateForTest(t)
	m3Wipe(t, st)
	t.Cleanup(func() { m3Wipe(t, st) })

	ctx := context.Background()
	res, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M3测试客户', '启用', 1, 1, ?)`, m3CustCode, m3StoreOpenID)
	if err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	custID, _ := res.LastInsertId()

	res, err = st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M3测试原料', '原料', '启用', 1, 1, ?)`, m3MatCode, m3StoreOpenID)
	if err != nil {
		t.Fatalf("建测试物料失败: %v", err)
	}
	matID, _ := res.LastInsertId()
	return st, custID, matID
}

func m3Actor() MDActor {
	return MDActor{OpenID: m3StoreOpenID, Name: "m3-store", Role: "receiver", IP: "127.0.0.1"}
}

// m3Notice 造一张预报（默认客供、预计袋数 0）。
func m3Notice(t *testing.T, st *Store, custID, matID int64) ArrivalNotice {
	t.Helper()
	n, err := st.CreateNotice(context.Background(), NoticeInput{
		CustomerID:  custID,
		MaterialID:  matID,
		PlateNo:     "湘F·测试",
		Driver:      "司机甲",
		EstBagCount: 0,
		BizType:     BizTypeCG,
		ArriveDate:  m3Date,
	}, m3Actor())
	if err != nil {
		t.Fatalf("登记预报失败: %v", err)
	}
	return n
}

// m3Arrive 造一张车次（预报 → 到货确认）。
func m3Arrive(t *testing.T, st *Store, custID, matID int64) TruckLot {
	t.Helper()
	n := m3Notice(t, st, custID, matID)
	truck, err := st.ConfirmArrival(context.Background(), ArriveInput{NoticeID: n.ID}, m3Actor())
	if err != nil {
		t.Fatalf("到货确认失败: %v", err)
	}
	return truck
}

// TC-M3-04 正常：一天内同客户同物料录 3 次预报 ⇒ 车序 01 / 02 / 03。
func TestTC_M3_04_SeqDailySeries(t *testing.T) {
	st, cust, mat := m3Fixture(t)
	for i, want := range []int{1, 2, 3} {
		n := m3Notice(t, st, cust, mat)
		if n.SeqNo != want {
			t.Fatalf("第 %d 次预报车序应 %02d，实际 %02d", i+1, want, n.SeqNo)
		}
		if n.Status != NoticeStatusForecast {
			t.Fatalf("预报状态应「%s」，实际 %q", NoticeStatusForecast, n.Status)
		}
	}
}

// TC-M3-05 ★ 边界：预报 02 的车没来（取消）⇒ 之后再录一车是 04，**不回收 02**。
func TestTC_M3_05_GapNotRecycled(t *testing.T) {
	st, cust, mat := m3Fixture(t)
	first := m3Notice(t, st, cust, mat)
	second := m3Notice(t, st, cust, mat)
	third := m3Notice(t, st, cust, mat)
	if first.SeqNo != 1 || second.SeqNo != 2 || third.SeqNo != 3 {
		t.Fatalf("前三车应为 1/2/3，实际 %d/%d/%d", first.SeqNo, second.SeqNo, third.SeqNo)
	}

	// 02 号车没来 ⇒ 作废该序号（status 置「空号」，行保留）
	canceled, err := st.CancelNotice(context.Background(), second.ID, NoticeStatusVacant,
		"车辆未到", m3Actor())
	if err != nil {
		t.Fatalf("取消预报失败: %v", err)
	}
	if canceled.Status != NoticeStatusVacant {
		t.Fatalf("状态应「%s」，实际 %q", NoticeStatusVacant, canceled.Status)
	}
	if canceled.SeqNo != 2 {
		t.Fatalf("作废的序号应保持 02，实际 %02d", canceled.SeqNo)
	}

	fourth := m3Notice(t, st, cust, mat)
	if fourth.SeqNo != 4 {
		t.Fatalf("新车序应 04（02 不回收），实际 %02d", fourth.SeqNo)
	}
}

// TC-M3-06 ★ 并发：两人同时给同客户同物料同日录预报 ⇒ 两个不同车序，无重号。
//
// ★ 取号在「命名锁 + 同事务内 FOR UPDATE」下串行（spec#rules.concurrent_alloc）。
func TestTC_M3_06_ConcurrentSeqNoDuplicate(t *testing.T) {
	st, cust, mat := m3Fixture(t)

	const workers = 4
	seqs := make([]int, workers)
	errs := make([]error, workers)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := 0; i < workers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			n, err := st.CreateNotice(context.Background(), NoticeInput{
				CustomerID: cust, MaterialID: mat, BizType: BizTypeCG,
				ArriveDate: m3Date, PlateNo: fmt.Sprintf("湘F·并发%d", i),
			}, m3Actor())
			if err == nil {
				seqs[i] = n.SeqNo
			}
			errs[i] = err
		}(i)
	}
	start.Done()
	done.Wait()

	seen := map[int]bool{}
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个并发登记失败: %v", i+1, errs[i])
		}
		if seen[seqs[i]] {
			t.Fatalf("并发取号重号：第 %d 个也拿到 %02d（全量 %v）", i+1, seqs[i], seqs)
		}
		seen[seqs[i]] = true
	}
	if len(seen) != workers {
		t.Fatalf("应拿到 %d 个不同车序，实际 %v", workers, seqs)
	}
}

// TC-M3-09 ★ 正常：净重 30.5 吨、30 袋 ⇒ 生成 30 个袋码，
// bag_count=30、weight_allocated=1.017（列精度 3 位）、weight_is_allocated=1。
func TestTC_M3_09_WeightAllocation(t *testing.T) {
	st, cust, mat := m3Fixture(t)
	ctx := context.Background()
	truck := m3Arrive(t, st, cust, mat)

	weighed, err := st.WeighTruck(ctx, truck.ID, WeighInput{
		Gross: tons{milli: 35500}, Tare: tons{milli: 5000},
	}, m3Actor())
	if err != nil {
		t.Fatalf("过磅失败: %v", err)
	}
	if weighed.NetWeight == nil || *weighed.NetWeight != 30.5 {
		t.Fatalf("净重应 30.5，实际 %v", weighed.NetWeight)
	}

	bags, err := st.GenerateBags(ctx, truck.ID, 30, m3Actor())
	if err != nil {
		t.Fatalf("生成袋码失败: %v", err)
	}
	if len(bags) != 30 {
		t.Fatalf("应生成 30 个袋码，实际 %d", len(bags))
	}
	after, err := st.GetTruck(ctx, truck.ID)
	if err != nil {
		t.Fatalf("读车次失败: %v", err)
	}
	if after.BagCount != 30 {
		t.Fatalf("bag_count 应 30，实际 %d", after.BagCount)
	}
	for _, b := range bags {
		if b.WeightAllocated == nil || *b.WeightAllocated != 1.017 {
			t.Fatalf("袋 %d 的 weight_allocated 应 1.017，实际 %v", b.BagSeq, b.WeightAllocated)
		}
		if b.WeightIsAllocated != 1 {
			t.Fatalf("weight_is_allocated 恒为 1，实际 %d", b.WeightIsAllocated)
		}
	}
}

// TC-M3-10 ★★ 边界：预报 30 袋、实际 28 袋 ⇒ 只生成 28 个袋码；
// 预报后 / 到货确认后 / 过磅后（未录袋数）查 b_bag 均为空 —— **不按预报袋数预打**。
func TestTC_M3_10_NoBagBeforeWeighConfirm(t *testing.T) {
	st, cust, mat := m3Fixture(t)
	ctx := context.Background()
	countBags := func() int {
		t.Helper()
		var n int
		// ★ 只数**本用例自己造的袋**：库里可能同时有别的批次/别的账号的数据，
		//	「预报阶段无袋码」是相对本条业务链的断言，不能拿全库计数。
		if err := st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM b_bag WHERE created_by = ?`, m3StoreOpenID).Scan(&n); err != nil {
			t.Fatalf("统计袋数失败: %v", err)
		}
		return n
	}

	// ① 预报阶段（预计 30 袋）：无任何袋码
	n, err := st.CreateNotice(ctx, NoticeInput{
		CustomerID: cust, MaterialID: mat, BizType: BizTypeCG,
		ArriveDate: m3Date, EstBagCount: 30,
	}, m3Actor())
	if err != nil {
		t.Fatalf("登记预报失败: %v", err)
	}
	if got := countBags(); got != 0 {
		t.Fatalf("预报阶段 b_bag 应为空，实际 %d 行", got)
	}

	// ② 到货确认后：仍无袋码（车码 A 有了，袋码 B 还没有）
	truck, err := st.ConfirmArrival(ctx, ArriveInput{NoticeID: n.ID}, m3Actor())
	if err != nil {
		t.Fatalf("到货确认失败: %v", err)
	}
	if got := countBags(); got != 0 {
		t.Fatalf("到货确认后 b_bag 应为空，实际 %d 行", got)
	}

	// ③ 过磅后但未录实际袋数：仍无袋码
	if _, err := st.WeighTruck(ctx, truck.ID, WeighInput{
		Gross: tons{milli: 31000}, Tare: tons{milli: 1000},
	}, m3Actor()); err != nil {
		t.Fatalf("过磅失败: %v", err)
	}
	if got := countBags(); got != 0 {
		t.Fatalf("过磅后（未录袋数）b_bag 应为空，实际 %d 行", got)
	}

	// ④ 录**实际** 28 袋 ⇒ 只生成 28 个
	bags, err := st.GenerateBags(ctx, truck.ID, 28, m3Actor())
	if err != nil {
		t.Fatalf("生成袋码失败: %v", err)
	}
	if len(bags) != 28 || countBags() != 28 {
		t.Fatalf("应只生成 28 个袋码（预报 30），实际 len=%d count=%d", len(bags), countBags())
	}
	if bags[0].BagSeq != 1 || bags[27].BagSeq != 28 {
		t.Fatalf("袋序应 1..28，实际首=%d 末=%d", bags[0].BagSeq, bags[27].BagSeq)
	}
	after, _ := st.GetTruck(ctx, truck.ID)
	if after.BagCount != 28 {
		t.Fatalf("bag_count 应 28，实际 %d", after.BagCount)
	}
}
