package store

// ===== N-023 收货批次化 · 库侧 TC =====
//
// ★ DB 集成测试：本机未设 JX_TEST_DB=1 一律 Skip（docs/05），服务器上经
//	scripts/run_tc_server.sh 以 JX_TEST_DB=1 执行。
// ★ 覆盖任务包 D5：①三元组唯一 ②关闭幂等+关闭后预报被拒 ③L1/L2/L3 口径
//	（含旧版排除/标注）④车序批次内递增+旧行不动 ⑤到货确认写链根日期。

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	n023StoreOpenID = "ou_test_n023_store"
	n023CustCode    = "9303"
	n023MatCode     = "9303"
)

func n023Actor() MDActor {
	return MDActor{OpenID: n023StoreOpenID, Name: "n023-store", Role: "receiver", IP: "127.0.0.1"}
}

// n023Wipe 清场（先子后父、按账号精确匹配）。
func n023Wipe(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	me := n023StoreOpenID
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM b_obj_void WHERE entity = 'b_bag'
	     AND entity_id IN (SELECT id FROM b_bag WHERE created_by = ?)`, me)
	q(`DELETE FROM b_bag WHERE created_by = ?`, me)
	q(`DELETE FROM b_truck_lot WHERE created_by = ?`, me)
	q(`DELETE FROM b_arrival_notice WHERE created_by = ?`, me)
	q(`DELETE FROM m_incoming_batch WHERE created_by = ?`, me)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, n023CustCode)
	q(`DELETE FROM m_material WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, n023MatCode)
}

// n023Fixture 清场 + 建主数据，返回 (st, custID, matID)。
func n023Fixture(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	st := migrateForTest(t)
	n023Wipe(t, st)
	t.Cleanup(func() { n023Wipe(t, st) })
	ctx := context.Background()
	res, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'N023测试客户', '启用', 1, 1, ?)`, n023CustCode, n023StoreOpenID)
	if err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	custID, _ := res.LastInsertId()
	res, err = st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'N023测试原料', '原料', '启用', 1, 1, ?)`, n023MatCode, n023StoreOpenID)
	if err != nil {
		t.Fatalf("建测试物料失败: %v", err)
	}
	matID, _ := res.LastInsertId()
	return st, custID, matID
}

func n023Batch(t *testing.T, st *Store, custID, matID int64, cbn string) IncomingBatch {
	t.Helper()
	b, err := st.CreateBatch(context.Background(), BatchInput{
		CustomerID: custID, MaterialID: matID, CustomerBatchNo: cbn,
	}, n023Actor())
	if err != nil {
		t.Fatalf("预填批次失败: %v", err)
	}
	return b
}

// D5-1 ★ 三元组唯一：重复预填 ⇒ ErrBatchDuplicate 且携带已有批次的标识。
func TestN023_BatchTripleUnique(t *testing.T) {
	st, cust, mat := n023Fixture(t)
	ctx := context.Background()
	first := n023Batch(t, st, cust, mat, "N023-DUP-01")
	if !strings.HasPrefix(first.OurBatchNo, "1F") || len(first.OurBatchNo) != 27 {
		t.Fatalf("我方批次号应为 27 位 F 码（1F 开头），实际 %q", first.OurBatchNo)
	}
	if first.Status != BatchStatusOpen {
		t.Fatalf("新批次状态应「%s」，实际 %q", BatchStatusOpen, first.Status)
	}

	_, err := st.CreateBatch(ctx, BatchInput{
		CustomerID: cust, MaterialID: mat, CustomerBatchNo: "N023-DUP-01",
	}, n023Actor())
	if !errors.Is(err, ErrBatchDuplicate) {
		t.Fatalf("重复三元组应报 ErrBatchDuplicate，实际 %v", err)
	}
	var dup *BatchDuplicateError
	if !errors.As(err, &dup) || len(dup.Existing) == 0 {
		t.Fatalf("错误应携带已有批次列表，实际 %v", err)
	}
	if dup.Existing[0].OurBatchNo != first.OurBatchNo ||
		dup.Existing[0].CustomerBatchNo != "N023-DUP-01" {
		t.Fatalf("已有批次标识不符: %+v", dup.Existing[0])
	}

	// 不同客户批次号（同客户同产品）⇒ 合法（时间戳区分）
	second := n023Batch(t, st, cust, mat, "N023-DUP-02")
	if second.OurBatchNo == first.OurBatchNo {
		t.Fatalf("两个批次的 F 码应不同（时间戳），实际都是 %q", first.OurBatchNo)
	}
}

// D5-2 ★ 关闭幂等；关闭后预报新车次 ⇒ ErrBatchClosed（400）；
// 已有关联车次数据不受影响。
func TestN023_BatchCloseIdempotentAndBlocksForecast(t *testing.T) {
	st, cust, mat := n023Fixture(t)
	ctx := context.Background()
	batch := n023Batch(t, st, cust, mat, "N023-CLOSE-01")

	// 关联一条已到货车次（关闭不得破坏既有数据）
	n, err := st.CreateNotice(ctx, NoticeInput{BatchID: batch.ID, PlateNo: "湘F·关"}, n023Actor())
	if err != nil {
		t.Fatalf("登记预报失败: %v", err)
	}
	truck, err := st.ConfirmArrival(ctx, ArriveInput{NoticeID: n.ID}, n023Actor())
	if err != nil {
		t.Fatalf("到货确认失败: %v", err)
	}

	// 关闭
	closed, err := st.CloseBatch(ctx, batch.ID, n023Actor())
	if err != nil {
		t.Fatalf("关闭批次失败: %v", err)
	}
	if closed.Status != BatchStatusClosed || closed.ClosedAt == nil || closed.ClosedBy != n023StoreOpenID {
		t.Fatalf("关闭留痕不符: %+v", closed)
	}
	firstClosedAt := *closed.ClosedAt

	// 幂等：再关一次 ⇒ 200 同值（closed_at 不变）
	again, err := st.CloseBatch(ctx, batch.ID, n023Actor())
	if err != nil {
		t.Fatalf("重复关闭应幂等成功: %v", err)
	}
	if again.Status != BatchStatusClosed || again.ClosedAt == nil || !again.ClosedAt.Equal(firstClosedAt) {
		t.Fatalf("幂等关闭不应改写 closed_at：first=%v again=%v", firstClosedAt, again.ClosedAt)
	}

	// 关闭后预报 ⇒ 400
	_, err = st.CreateNotice(ctx, NoticeInput{BatchID: batch.ID}, n023Actor())
	if !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("关闭后预报应报 ErrBatchClosed，实际 %v", err)
	}
	// 关闭后内联补录预报（无预报直接到货）同样被拒
	_, err = st.ConfirmArrival(ctx, ArriveInput{Inline: &NoticeInput{BatchID: batch.ID}}, n023Actor())
	if !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("关闭后内联补录应报 ErrBatchClosed，实际 %v", err)
	}
	// 已有关联车次不受影响
	got, err := st.GetTruck(ctx, truck.ID)
	if err != nil || got.Code == "" {
		t.Fatalf("关闭后既有车次应完好: err=%v got=%+v", err, got)
	}
}

// D5-4 ★ 车序批次内递增；同客户同产品**不同批次**各自从 01 起；
// 旧行（batch_id NULL 存量预报）不参与新取号、seq_no 不动。
func TestN023_SeqPerBatchAndLegacyUntouched(t *testing.T) {
	st, cust, mat := n023Fixture(t)
	ctx := context.Background()

	// 造一行旧版预报（batch_id NULL —— 模拟 N-023 前的存量行）
	res, err := st.DB().ExecContext(ctx, `
INSERT INTO b_arrival_notice
  (notice_no, customer_id, material_id, biz_type, seq_no, arrive_date, status, created_by)
VALUES ('YB26100999legacy', ?, ?, 'CG', 42, '2026-10-09', '预报', ?)`,
		cust, mat, n023StoreOpenID)
	if err != nil {
		t.Fatalf("造旧版预报失败: %v", err)
	}
	legacyID, _ := res.LastInsertId()

	b1 := n023Batch(t, st, cust, mat, "N023-SEQ-A")
	b2 := n023Batch(t, st, cust, mat, "N023-SEQ-B")

	// 批次 A 内 1/2/3；批次 B 独立从 1 起（不与 A 混号）
	for i, want := range []int{1, 2, 3} {
		n, err := st.CreateNotice(ctx, NoticeInput{BatchID: b1.ID, PlateNo: "湘F·A"}, n023Actor())
		if err != nil {
			t.Fatalf("A 批第 %d 次预报失败: %v", i+1, err)
		}
		if n.SeqNo != want {
			t.Fatalf("A 批第 %d 次车序应 %02d，实际 %02d", i+1, want, n.SeqNo)
		}
		if n.BatchID != b1.ID || n.OurBatchNo != b1.OurBatchNo {
			t.Fatalf("预报应挂批次并带出我方批次号: %+v", n)
		}
	}
	nB, err := st.CreateNotice(ctx, NoticeInput{BatchID: b2.ID, PlateNo: "湘F·B"}, n023Actor())
	if err != nil {
		t.Fatalf("B 批预报失败: %v", err)
	}
	if nB.SeqNo != 1 {
		t.Fatalf("B 批首车序应 01（独立取号），实际 %02d", nB.SeqNo)
	}

	// 旧版权威行不动
	var legacySeq int
	var legacyBatch sql.NullInt64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT seq_no, batch_id FROM b_arrival_notice WHERE id = ?`, legacyID).
		Scan(&legacySeq, &legacyBatch); err != nil {
		t.Fatalf("读旧版预报失败: %v", err)
	}
	if legacySeq != 42 || legacyBatch.Valid {
		t.Fatalf("旧版预报应保持 seq=42 / batch_id=NULL，实际 seq=%d batch_id valid=%v",
			legacySeq, legacyBatch.Valid)
	}
}

// D5-5 ★ 到货确认写链根日期：新预报创建时 arrive_date 为空，确认后写入且与车次/车码一致。
func TestN023_ArrivalWritesChainRootDate(t *testing.T) {
	st, cust, mat := n023Fixture(t)
	ctx := context.Background()
	batch := n023Batch(t, st, cust, mat, "N023-ROOT-01")

	n, err := st.CreateNotice(ctx, NoticeInput{BatchID: batch.ID}, n023Actor())
	if err != nil {
		t.Fatalf("登记预报失败: %v", err)
	}
	if n.ArriveDate != "" {
		t.Fatalf("创建时不应写链根日期（表单已无该输入），实际 %q", n.ArriveDate)
	}

	// 用固定确认时刻 ⇒ 链根日期 = 该日期（可重复断言）
	at := time.Date(2026, 10, 9, 10, 30, 0, 0, time.Local)
	truck, err := st.ConfirmArrival(ctx, ArriveInput{NoticeID: n.ID, ArriveAt: &at}, n023Actor())
	if err != nil {
		t.Fatalf("到货确认失败: %v", err)
	}
	if truck.ArriveDate != "2026-10-09" {
		t.Fatalf("车次链根日期应 2026-10-09，实际 %q", truck.ArriveDate)
	}
	after, err := st.GetNotice(ctx, n.ID)
	if err != nil {
		t.Fatalf("读预报失败: %v", err)
	}
	if after.ArriveDate != "2026-10-09" {
		t.Fatalf("预报链根日期应由到货确认写入 2026-10-09，实际 %q", after.ArriveDate)
	}
	// 车码 DATE 段 = 链根日期 261009（第13~18位）
	if len(truck.Code) != 27 || truck.Code[12:18] != "261009" {
		t.Fatalf("车码 DATE 段应 261009，实际码 %q", truck.Code)
	}
}

// L1 统计口径：车次数=预报+已到货；已到货=车次行；已投完=有效袋全部已投料。
func TestN023_L1BatchStats(t *testing.T) {
	st, cust, mat := n023Fixture(t)
	ctx := context.Background()
	batch := n023Batch(t, st, cust, mat, "N023-STAT-01")

	// 车1：到货 + 过磅 + 2 袋 + 全部投料 ⇒ 已投完
	n1, _ := st.CreateNotice(ctx, NoticeInput{BatchID: batch.ID}, n023Actor())
	truck1, err := st.ConfirmArrival(ctx, ArriveInput{NoticeID: n1.ID}, n023Actor())
	if err != nil {
		t.Fatalf("到货确认1失败: %v", err)
	}
	if _, err := st.WeighTruck(ctx, truck1.ID, WeighInput{
		Gross: tons{milli: 30000}, Tare: tons{milli: 5000},
	}, n023Actor()); err != nil {
		t.Fatalf("过磅失败: %v", err)
	}
	if _, err := st.GenerateBags(ctx, truck1.ID, 2, n023Actor()); err != nil {
		t.Fatalf("生成袋码失败: %v", err)
	}
	// 直写库把袋置「已投料」（投料属 M6，此处只造状态）
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE b_bag SET status = '已投料' WHERE truck_lot_id = ?`, truck1.ID); err != nil {
		t.Fatalf("置已投料失败: %v", err)
	}

	// 车2：仅预报（未到货）
	if _, err := st.CreateNotice(ctx, NoticeInput{BatchID: batch.ID}, n023Actor()); err != nil {
		t.Fatalf("登记预报2失败: %v", err)
	}
	// 车3：到货但未生成袋 ⇒ 不计已投完
	n3, _ := st.CreateNotice(ctx, NoticeInput{BatchID: batch.ID}, n023Actor())
	if _, err := st.ConfirmArrival(ctx, ArriveInput{NoticeID: n3.ID}, n023Actor()); err != nil {
		t.Fatalf("到货确认3失败: %v", err)
	}
	// 车4：到货 + 袋作废（有效袋=0）⇒ 不计已投完
	n4, _ := st.CreateNotice(ctx, NoticeInput{BatchID: batch.ID}, n023Actor())
	truck4, err := st.ConfirmArrival(ctx, ArriveInput{NoticeID: n4.ID}, n023Actor())
	if err != nil {
		t.Fatalf("到货确认4失败: %v", err)
	}
	if _, err := st.WeighTruck(ctx, truck4.ID, WeighInput{
		Gross: tons{milli: 20000}, Tare: tons{milli: 5000},
	}, n023Actor()); err != nil {
		t.Fatalf("过磅4失败: %v", err)
	}
	if _, err := st.GenerateBags(ctx, truck4.ID, 1, n023Actor()); err != nil {
		t.Fatalf("生成袋码4失败: %v", err)
	}
	if _, err := st.VoidBag(ctx, bagIDOf(t, st, truck4.ID), "作废口径测试", n023Actor()); err != nil {
		t.Fatalf("作废袋失败: %v", err)
	}
	// 一条已取消预报 ⇒ 不计入车次数（有效车次口径）
	n5, _ := st.CreateNotice(ctx, NoticeInput{BatchID: batch.ID}, n023Actor())
	if _, err := st.CancelNotice(ctx, n5.ID, NoticeStatusCanceled, "取消测试", n023Actor()); err != nil {
		t.Fatalf("取消预报失败: %v", err)
	}

	got, err := st.GetBatch(ctx, batch.ID)
	if err != nil {
		t.Fatalf("读批次失败: %v", err)
	}
	// 车次数 = 预报+已到货 = 4（取消的 1 条不计）
	if got.NoticeCount != 4 {
		t.Errorf("车次数应 4（取消不计），实际 %d", got.NoticeCount)
	}
	// 已到货 = 3（车1/3/4）
	if got.ArrivedCount != 3 {
		t.Errorf("已到货应 3，实际 %d", got.ArrivedCount)
	}
	// 已投完 = 1（只有车1：有效袋>0 且全投料）
	if got.FedDoneCount != 1 {
		t.Errorf("已投完应 1，实际 %d", got.FedDoneCount)
	}
	if !(got.FedDoneCount <= got.ArrivedCount && got.ArrivedCount <= got.NoticeCount) {
		t.Errorf("统计应呈漏斗 已投完≤已到货≤车次数：%d/%d/%d",
			got.FedDoneCount, got.ArrivedCount, got.NoticeCount)
	}
	_ = n1
}

// bagIDOf 取车次下第一个袋 id（直写库测试的辅助）。
func bagIDOf(t *testing.T, st *Store, truckID int64) int64 {
	t.Helper()
	var id int64
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT id FROM b_bag WHERE truck_lot_id = ? ORDER BY bag_seq LIMIT 1`, truckID).Scan(&id); err != nil {
		t.Fatalf("查袋失败: %v", err)
	}
	return id
}
