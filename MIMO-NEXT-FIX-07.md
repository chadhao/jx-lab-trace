# MIMO-NEXT-FIX-07 —— 收货模块批次化改造（N-023，一次交付）

> 交付方：**mimo code**　提出方：WorkBuddy　对应议题：**`N-023`**
> ★ 来源：与项目总经理五轮问答定案（2026-10-12～14，`docs/21` 同期）。**业务口径见本文件「§0 定案」，全部已获用户确认，不要再问。**

---

## §0 业务定案（用户逐条确认，实现必须忠实于此）

1. **新实体「来料批次」**：客户下单后预填（客户、产品、客户批次号）。**一个批次＝同客户同产品的多台车**。我方批次号**自动生成（含时间戳，挂 code-rules 新码类）**，天然唯一；**唯一约束＝(customer_id, material_id, customer_batch_no)**，重复预填 ⇒ 报错并列出已有批次供跳转。
2. **车次预报改造**：不填客户/产品/预计到货时间/到货日/袋数预估 —— **只选批次**（筛客户＋产品，**仅「回货中」**）＋ 车辆信息（车牌/司机/电话）。
3. **到货日＝到货确认时刻**，机制不变：`b_arrival_notice.arrive_date`（链根日期）**列保留**、到货确认写入，**删的是表单输入项**；`eta`、`est_bag_count` 两列删除。
4. **「回完货」＝手工关闭批次**（不自动判定）。关闭后不能再预报新车次；已有关联车次/到货/检测数据不受影响。关闭动作留痕（closed_at/by）。
5. **投料进度仅展示**：车次所有吨袋扫码投完＝该车「已投完」（`b_bag.status='已投料'` 按 truck_lot 统计），批次级显示「已投 x/y 车」。**不驱动批次状态**（Q1 手工关闭）。
6. **三个列表**：L1 未关闭批次（回货中，含进度）；L2 未到货车次（预报未确认）；L3 全量查询（批次＋车次，多条件，含旧版预报标注）。
7. **系统无预估**：凡预估字段一律删除，一切以扫码实际为准。
8. **权限沿用收货域现有权限点，不新增**（关闭批次属管理动作，挂点由 mimo 按现有点选并说明）。
9. **完整流程**（用户原话）：①客户下单预填批次 → ②客户派车预填车辆 → ③到货点选预填车辆补重量入库生成唯一编码（含二维码）→ ④原料取样检测 → ⑤生产投料 → ⑥过程检测 → ⑦成品装袋检测 → ⑧发货。

---

## D1 · 数据模型与迁移

### D1.1 新表 `m_incoming_batch`
```sql
CREATE TABLE m_incoming_batch (
  id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  our_batch_no       CHAR(27)     NOT NULL COMMENT '我方批次号（新码类，含时间戳）',
  customer_batch_no  VARCHAR(64)  NOT NULL COMMENT '客户提供的批次号',
  customer_id        BIGINT UNSIGNED NOT NULL,
  material_id        BIGINT UNSIGNED NOT NULL,
  status             VARCHAR(16)  NOT NULL DEFAULT '回货中' COMMENT '回货中 / 已关闭',
  closed_at          DATETIME(3)  NULL,
  closed_by          VARCHAR(64)  NULL,
  created_at         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by         VARCHAR(64)  NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_our_batch_no (our_batch_no),
  UNIQUE KEY uk_batch_triple (customer_id, material_id, customer_batch_no),
  KEY idx_batch_status (status)
) COMMENT='来料批次（客户下单预填；一批次同产品多车）';
```
（列长/码长按 code-rules 现有风格微调，**唯一三元组不可变**。）

### D1.2 `b_arrival_notice` 改造
- ＋ `batch_id BIGINT UNSIGNED NULL`（**新建预报必填**；旧行为 NULL＝「旧版预报」）
- **DROP COLUMN `eta`**、**DROP COLUMN `est_bag_count`**
- **`arrive_date` 列保留**（NULL，到货确认写入链根——**机制一字不改**）
- 索引 `idx_notice_seq` → **`idx_batch_seq (batch_id, seq_no)`**；★ 车序 `seq_no` 新行按**批次内递增**（跳号不回收的现有规则沿用）；旧行 seq_no 不动。
- **迁移**：既有行 `batch_id=NULL`（旧版）；`eta/est_bag_count` 值随列删除自然消失；**旧预报行与已到货车次/检测链一律不删**。

### D1.3 spec 同步（★ 双引擎一致：`internal/specload` 与 `scripts/check_spec.py` 都要过）
- `spec/schema.sql`：上述 DDL。
- `spec/code-rules.json`：**新码类**（我方批次号）——字母从现有占用外选（A=车次/B=吨袋/D=成品批…），含时间戳段；登记完整规则。
- `spec/enums.json`：批次状态（回货中/已关闭）。
- `spec/forms/`：收货模块表单改造（批次预报单新表单；车次预报表单去 eta/到货日/袋数、加批次选择）。
- `spec/checks.json`：新增判据——批次三元组唯一、关闭后不可预报新车次、L1/L2/L3 口径、旧版预报排除规则、**链根日期仍由到货确认写入**（防回归）。

---

## D2 · API（语义契约；具体路由风格按现有 recv.go）

- `POST /api/recv/batches`：预填批次（三元组重复 ⇒ 400，**响应体含已有批次的 our_batch_no/customer_batch_no** 供跳转）
- `GET  /api/recv/batches?status=回货中`：**L1**（含每批次统计：车次数/已到货/已投完车数）
- `POST /api/recv/batches/:id/close`：**手工关闭**（幂等；关闭后预报新车次 ⇒ 400 明确报错）
- `GET  /api/recv/batches/all`：**L3 批次侧**（筛选：customer/material/customer_batch_no/our_batch_no/status；含旧版预报标注）
- `POST /api/recv/notices`：**改造**——必带 `batch_id`，不再接受 eta/est_bag_count/arrive_date 输入；customer/material 从批次带出；**批次已关闭 ⇒ 400**
- `GET  /api/recv/notices?pending=1`：**L2**（batch_id 非空；含所属批次号）
- `GET  /api/recv/search`：**L3 统一查询**（批次＋车次，多条件；旧版预报标注「旧版」）
- ★ 到货确认 `POST /arrivals` **行为不变**（生成 27 位车次码/二维码、写链根 arrive_date/at）。
- ★ **投料进度统计**：批次下 `b_bag`（经车次）`status='已投料'` 占比，聚合口径在回执写明。

## D3 · 前端（`Receiving.vue` 改造）

- **页签结构改为三列表**：①未关闭批次（L1，含进度、关闭按钮、继续预报）②未到货车次（L2）③查询（L3）
- **批次预报表单**（新）：客户/产品（下拉）/客户批次号 ⇒ 提交显示我方批次号
- **车次预报表单**（改造）：批次下拉（筛客户＋产品，**仅回货中**；含我方批次号＋客户批次号便于辨认）＋ 车牌/司机/电话
- **旧版预报**在 L3 中展示但**不可编辑**（标注「旧版」）
- ★ 全部走现有 api.js；浅色主题沿用。

## D4 · 事件与既有链（★ 回归红线）

- 飞书事件回调（created/deleted）、`/feishu/event`、组织同步：**不受影响**（回归测试跑一遍）。
- 到货确认 → 27 位码 → 吨袋摊算 → 取样检测 → 投料 → 追溯：**全链回归**（trace 前向/反向、报告快照）。
- **链根日期仍由到货确认写入**（checks 判据钉住）。

## D5 · 测试（缺一不可）

1. 三元组唯一（重复 ⇒ 400 含已有批次标识）
2. 关闭后预报新车次 ⇒ 400；关闭幂等
3. L1/L2/L3 口径（含旧版排除/标注）
4. 车序批次内递增；旧行不动
5. 到货确认写链根日期（回归）
6. **双引擎**：`check_spec.py` 与 `specload` 对新 checks 一致
7. 全链回归（到货→检测→投料→追溯→报告）
8. **形状断言**：新/改端点请求响应逐字段

## D6 · 交付

- ★ 门禁 **11/11**（含新判据的双引擎）；部署测试服务器（**部署前确认 `.env` 完好、部署后 healthz 版本＝本次提交无 dirty**——N-020 红线）。
- ★ **curl 实测**：预填批次→车次预报→列表三口径→关闭→关闭后预报被拒，全链走一遍（真实数据）。
- ★ 回执写 `COLLAB.md` **`### N-023` 段内**、状态 **`MIMO-DONE`**；显式路径提交；**只本地提交不 push**；如实声明未做项。

---

## §6 口径要求（硬约束）

1. ★★★ **系统调试与验收一律在测试服务器**；本机只编译/纯单测/门禁。部署前确认 `.env` 完好，**部署链路不得触碰 `.env`**（N-020 红线）。
2. ★★★ **`spec/` 与双引擎同步改**：新判据两侧一致，`check_spec.py`/`specload` 任一单独过不算。
3. ★★ **不新增权限点**；关闭批次挂现有管理/收货点并说明。
4. ★★ **链根日期机制一字不改**；**动作型权限点不用于 GET 列表守卫**（N-021 教训）。
5. ★★ **`COLLAB.md` 局部追加**；时间戳取机器时钟；引号纪律；**编辑前先 Read**（CRLF）。
6. ★ **接口清单核对从路由注册表来，不从源码 grep**（N-021 教训）。
7. ❌ 不动 `docs/*` 冻结口径；❌ 不跳过/改期望任何失败测试；❌ 不删任何既有检测链数据。

## §8 易错点

- ★ `b_arrival_notice.arrive_date` 是**链根日期**（到货确认写入），别当预报输入删列。
- ★ 车序 `seq_no` 分组维度从 (customer,material,arrive_date) 换成 batch_id —— 旧行的 seq_no 语义不同，**查询区分新旧**。
- ★ L1 的「已投完 y」来自 `b_bag.status='已投料'` 按 truck_lot 聚合 —— 吨袋有作废/退回，**分母口径**（有效袋）在回执写明。
- ★ 用户在权限矩阵新配的授权（28 条 perm_change）是生效数据，**不要动**。
