# MIMO-NEXT-BATCH-06 · 批 6：M6 生产与谱系

> **交付方**：mimo code ｜ **验收方**：WorkBuddy｜ **派出日**：2026-10-09
> **批次**：批 6 / 8 ｜ **模块**：**M6 生产与谱系** ｜ **依赖**：批 1（M0 地基）+ 批 2（M1 主数据）+ 批 3（M3 收货与打码）+ 批 4（M4 取样与留样）+ 批 5（M5 检测）—— **五者均已独立验收通过 `AGREED`**（`N-001` / `N-007` / `N-010` / `N-011` / `N-012`）
> ★ **开工前必须完整读 `MIMO-ONBOARDING.md`**（含铁律与禁止事项），再读本文件。

---

## 0. 本轮说明（先读这段再读 §1）

> **本轮是「从零开工」，不是续派。** 工作区干净、`HEAD == origin/main == 23e9c2c`、门禁 **11/11** 全绿（WorkBuddy 于 2026-10-09 12:2x 实测）。

★ **但请你务必带上批 5 换来的两条纪律**（见 `COLLAB.md §0` 附加铁律 11 / 12）：

| # | 纪律 | 为什么 |
|---|---|---|
| **A** | **分阶段本地提交**：每完成一块（后端一块 / 服务器 TC 全绿 / 变异自证 / 前端）**立刻用显式路径提交一次**，**不要攒到最后一次性提交** | 派工轮随心跳 turn 结束被整树回收是**常态风险**（批 5 首次派工就撞上，25 分钟工作量零提交）；分批提交后**任何一次回收最多只丢最后一块** |
| **B** | **不要用 `git stash` / `git checkout` 处理未提交的 WIP** | 本仓库 `core.autocrlf=true` 且无 `.gitattributes` ⇒ stash 落成 **CRLF** ⇒ `gofmt -l` 判红、门禁 1/11 失败（2026-10-09 实测踩中） |

★ **本条不改变**「只本地提交、不 push」（硬约束二）。

---

## 1. 本批目标（一句话）

**把「这车料去了哪些成品」和「这批成品用了哪些料」都答得出**：建生产批 → **投料扫码写 `b_feed_record`（谱系承重墙）** → 作业段（跨班组）→ 成品批 / 成品袋生成与打码 → 谱系数据可查 → 返工（新批号 + 关联原批）。

★ 本批**只做生产与谱系**：**不做出货**（批 7 · M7）· **不做追溯页与批次档案页**（批 7 · M8）· **不做报告分享 / 报表**（批 8）。
★ 本批**最重要的四条**（做错就要返工）：
1. ★★ **投料扫码必须写 `b_feed_record`**，且**谱系 = 多对多，绝不编进码**（一车可拆多批、多车可并入一批）；
2. ★★ **未出结论的料不得投料** —— 判据是**袋所属车次的「现行检测单」`conclusion ∈ {合格, CONCESSION}`**，或该车次有**生效的紧急放行**（★ 生效＝`s_audit_log` 上 **init + approve 两笔齐、且两人不同**；只有 init ⇒ **不生效**）；
3. ★★ **`D`/`E` 的序 1 必须是「来源生产批序」**，且 **`D`/`E` 的日期段 = 来源生产批的 `batch_date`（链根日期）** —— 成品袋码要**自带完整祖先链**（`docs/02` §2 / D11）；
4. ★★ **实际产出只在 `b_fg_lot` 记一次**（D23）—— `b_production_batch` **只留「计划产出物料」**，产出物料 / 净重 / 袋数 / 产出时间**一律不写生产批**。

---

## 2. 必读

| # | 文件 | 读什么 |
|---|---|---|
| 1 | `COLLAB.md` | §0 铁律（★ 含**附加铁律 6~12**）· §1 当前状态 · §2 责任域 · §4 **`N-008` / `N-013`** |
| 2 | `docs/04-模块设计与用例.md` | ★★ **`M6` 的 UC-M6-01~05 / TC-M6-01~09**（这是你的验收标准） |
| 3 | `docs/03-模块划分与实施批次.md` | **批 6 的 5 条验收要点**（§2 表格「批 6」行） |
| 4 | `docs/01-设计定案.md` | **§5.3 生产与谱系**（6 张表 + ★★「生产批 / 成品批 边界」表）· **§6.3 生产与投料**（流程）· **D11**（码结构，`D`/`E` 序 1）· **D13**（放行单位＝**车次**）· **D15**（袋作废：**已投料不得作废**）· **D18**（一车一物料 / 自购客户段 `0000`）· **D23**（实际产出唯一记录点）· **P1**（数据不可变）· §8 权限矩阵「生产」行 |
| 5 | `docs/02-追踪码规则.md` | ★★ §2（三层序对应）· §3（**示例向量**：`C`/`D`/`E` 三条）· §4（序号空间与取号规则）· §8（生成时机） |
| 6 | `spec/schema.sql` | 本批要动的 **6 张表**：`b_production_batch`(296) `b_batch_operation`(312) **`b_feed_record`(329)** `b_fg_lot`(344) `b_fg_bag`(363) `b_rework`(378)；另**只读**引用 `b_bag`(202) `b_truck_lot`(220) `b_obj_void`(277) `b_label_print`(263) `s_audit_log`(603) `m_customer`(27) `m_material`(83) `b_inspection`(474) `b_sample_group`(395) —— ★ **列注释里的枚举原词就是取值** |
| 7 | `spec/code-rules.json` | ★ `object_types`（**`C`/`D`/`E` 的 seq1/seq2/seq3 语义**）· `sequence_spaces`（**批序 / 成品批序 / 成品袋序**的 scope 与 max）· `sample_vectors`（`C`/`D`/`E` 三条**逐字可核算**）· `generation`（生成时机） |
| 8 | `spec/permission-points.json` | 51 个权限点（★ 本批消费其中的 **6 个 `prod.*`**，**不得新增**）—— ★ 注意 `prod.rework` 的级别含 **`INIT`** |
| 9 | `internal/codec/codec.go` | ★ 码引擎（`Generate` / `Parse` / `ToHuman` / `FromHuman` / `ObjectOf` / `Placeholder`）—— **必须复用**，不得另写 |
| 10 | `internal/store/receiving.go` · `internal/store/receiving_bag.go` | ★★ **工程范式**：`GET_LOCK` + 同事务取号（`receiving.go:299/906`）· 批量生成袋码（`GenerateBags:168`）· 摊算（毫吨整数做算术）· 作废与审计 |
| 11 | `internal/store/inspection.go` | ★★ **投料前置要复用它的「现行检测单」判定**（`currentInspectionIDTx:169`）与紧急放行落痕（`UrgentReleaseInit:1073` / `UrgentReleaseApprove:1105`）—— **不得另写一套** |
| 12 | `internal/store/m3_test.go` · `m4_test.go` · `m5_test.go` | ★ 测试夹具纪律与双层形态（store / httpapi） |
| 13 | `docs/05-环境与调试约定.md` | ★★ **调试只在测试服务器**（见 §6 硬约束一） |

★ 6 个 `prod.*` 权限点**已在 `spec` 登记、常量已在 `internal/permission/code.go:40-45` 声明**（`ProdBatchCreate` / `ProdFeedScan` / `ProdFeedCorrect` / `ProdOpLog` / `ProdFgGen` / `ProdRework`），你只需**消费**（路由层引用），**不要**改声明文件或新增点。

---

## 3. 交付物

### D1 · 建生产批（`prod.batch.create`）

- `POST /api/prod/batches`：body `{customer_id, input_material_id, planned_output_material_id, batch_date?, remark?}`。
  - `batch_date` 缺省＝**服务端当日**（★ 该字段即**链根日期**，`docs/02` §4-4）。
  - 校验：`customer_id` 存在且 `is_current=1` 且 `status='启用'`；`input_material_id` 须 `kind='原料'`；`planned_output_material_id` 须 **`kind='成品'`**（★ 不得复用 `lookupCodesCtx`，它硬编码 `kind='原料'` —— 见 §6-6）。
- ★★ **生产批码 `C`（`spec/code-rules.json#object_types.C`）**：`V=1` · `T=C` · `BT`（见 §6-5）· 客户段＝`m_customer.code`（4 位）· **物料段＝`planned_output_material_id` 的成品物料编号** · 日期段＝`batch_date`（`YYMMDD`）· **序1＝批序**（scope ＝ **客户 + 成品物料 + 创建日**，宽 2，**max 99**）· `序2=000` · `序3=000`。
  - 示例形态（`TC-M6-01`）：`1C-CG-0001-0003-261007-03-000-000-O`（人读行）／裸串 `1CCG0001000326100703000000`＋校验位 `O`。
  - ★ 取号手法 = `GET_LOCK`（命名锁）+ **同一事务**内按前缀取最大 +1（**禁止"先查最大值再 +1"**）；**超 99 明确报错**，**不自动进位**。
- `status` 建批为 **`进行中`**；`created_by` = 当前操作者。
- `GET /api/prod/batches`（列表，可按 `?status=` 过滤）/ `GET /api/prod/batches/:id`（详情）：权限 `prod.batch.create`，级别 **`LevelRead`**。
- 权限：写入口 `prod.batch.create`，级别 **`LevelAll`**。

### D2 · 投料扫码（`prod.feed.scan`）—— ★★ 谱系承重墙

- `POST /api/prod/batches/:id/feeds`：body `{bag_code, feed_weight?, fed_at?, remark?}`；`bag_code` 为**吨袋码（`B`）**裸串或人读行（★ 用 `codec.Parse` / `codec.FromHuman` 解析，**校验位不过即拒**）。
- ★★ **三项前置，缺一即拒**（详见 §6-11）：
  1. 袋必须存在，且 **`b_bag.status='在库'`** 且该 `bag_id` **无既有 `b_feed_record`**（★ 一袋只投一次，§6-13）；
  2. 该袋所属 **车次** 的**现行检测单** `conclusion ∈ {合格, CONCESSION}`（★ **复用 M5 的现行单判定**，`b_obj_void` 已作废单**不算**）；
  3. 或该车次存在**生效的紧急放行**（★ 生效判定见 §6-12）。
- 成功后：INSERT `b_feed_record(batch_id, bag_id, feed_weight, fed_at, operator, remark, created_by)`；★ **同事务**把 `b_bag.status` 置 **`已投料`**；★ 校验目标生产批 `status <> '已作废'`。
- `PATCH /api/prod/feeds/:id`（更正）：**只允许改** `feed_weight` / `remark`，**必须填 `reason`**（非空），写审计（`action='correct'`，`old_value` / `new_value`）。★ **不得**改 `batch_id` / `bag_id`（改它＝篡改谱系 ⇒ 走「删该条 + 重新扫码」）。
- `DELETE /api/prod/feeds/:id`（删除）：**必须填 `reason`**，**物理 `DELETE`** + 写审计（`action='delete'`，`old_value` 记整行快照）；★ 并把该袋 `status` **回置 `在库`**（同事务）。
- ★ 口径与理由见 §6-15。
- 权限：`prod.feed.scan`（级别 **`LevelAll`**）；更正 / 删除走 `prod.feed.correct`（级别 **`LevelAll`**）。

### D3 · 作业段（`prod.op.log`）

- `POST /api/prod/batches/:id/operations`：body `{team_id?, operator?, start_at?, end_at?, output_weight?, remark?}`。
  - `seq` **由服务端取号**（该 `batch_id` 内 +1，同事务；`uk_op_seq(batch_id, seq)` 兜底唯一）；超上限明确报错。
  - ★ **`output_weight` 是「本作业段产出（吨），非整批产出」**（`spec/schema.sql:320` 列注释原文）—— 不得当作整批产量。
- `GET /api/prod/batches/:id/operations`：列表（读，用 `prod.op.log` 的 `LevelRead`）。
- ★★ **跨班组就是多条作业段**（`TC-M6-06`）：批 = **业务概念**，班组 = **作业记录**；★ **不校验时间段重叠**、**不做自动结算**（无 UC 支撑，见铁律 7「不制造工作量」）。
- 权限：`prod.op.log`，级别 **`LevelAll`**。

### D4 · 成品批 / 成品袋生成与打码（`prod.fg.gen`）

- `POST /api/prod/batches/:id/fg-lots`（生成成品批）：body `{output_material_id?, pack_spec?, qty_bag?, net_weight?, produced_at?, remark?}`。
  - ★★ **`customer_id` 与 `output_material_id` 必须与来源生产批一致**（缺省即继承；显式传入但不一致 ⇒ **拒绝**，见 §6-7）；`output_material_id` 还须 `kind='成品'` 且 `is_current=1`、`status='启用'`。
  - ★★ **成品批码 `D`**：`T=D` · 客户段 / 物料段 / **日期段＝来源生产批 `batch_date`** · **序1＝来源生产批的批序** · **序2＝该生产批内第 n 个成品批**（宽 3，**max 999**） · `序3=000`。
    - 形态（`TC-M6-07`）：生产批序 `03` ⇒ 成品批码人读行 `1D-CG-0001-0003-261007-03-002-000-V`。
  - `status='在库'`。
- `POST /api/prod/fg-lots/:id/bags`（批量生成成品袋）：body `{count}`（正整数；上限见 `spec/code-rules.json#sequence_spaces.成品袋序.max = 999`）。
  - ★★ **成品袋码 `E`**：`T=E` · 段位**全部继承来源**（客户段 / 物料段 / **日期段＝生产批 `batch_date`** / **序1＝来源生产批序** / **序2＝成品批序**）· **序3＝本成品批内第 n 袋**（宽 3，max 999）。
  - `bag_seq` 从 **1** 起连续编号；`uk_fgbag_seq(fg_lot_id, bag_seq)` 兜底；★ **空号跳号不回收**（`docs/02` §4-1）。
  - `weight_allocated`：若成品批 `net_weight` 非空 ⇒ **均分摊算**（毫吨整数、half-up，沿用 M3 `GenerateBags` 手法），`weight_is_allocated=1`；否则**留空**且置 `0`（★ 不臆断）。
  - ★ 生成后**同事务**更新 `b_fg_lot.qty_bag` 为**实际生成数**。
- `GET /api/prod/batches/:id/fg-lots` / `GET /api/prod/fg-lots/:id/bags`：列表（读，`prod.fg.gen` 的 `LevelRead`）。
- `POST /api/prod/fg-lots/:id/print`（打印 / 补打）：body `{bag_seqs?: [...]}`，缺省＝全部；★ **写 `b_label_print`**（`code` = 成品袋码；**补打 `is_reprint=1` 且 `reason` 必填非空**；沿用 M3 口径）。★ 一期**不写打印驱动**，只出**标签版式数据**。
- 权限：`prod.fg.gen`，级别 **`LevelAll`**。

### D5 · 谱系数据可查（本批的「数据可查」下限，★ 不是 M8 追溯页）

> 依据：`docs/03` §2「批 6 验收要点」第 ② 条 —— **正向 / 反向谱系数据可查**。

- `GET /api/prod/batches/:id/feeds`（正向半步）：该批的投料明细 —— 袋码 / 车次码 / 客户 / 原料物料 / 投料量 / 投料时间 / 操作人。
- `GET /api/prod/batches/:id/fg-lots`（正向半步，D4 已列）：该批产出的成品批与成品袋。
- `GET /api/prod/genealogy/bags/:code`（反向半步）：某吨袋码 ⇒ 其投料记录 ⇒ 去向的生产批（可多条 ⇒ **多对多**）。
- ★ 口径：三条都只做**数据查询**（列表 + 关联码），**不做** M8 的「批次档案页 / 正反向互为逆的完整性断言 / 出货链」—— 那属批 7。
- 权限：读入口分别用 `prod.feed.scan`（投料与谱系）· `prod.fg.gen`（产出），级别 **`LevelRead`**。

### D6 · 返工（`prod.rework`）

- `POST /api/prod/rework`：body `{src_batch_id, reason, customer_id?, input_material_id?, planned_output_material_id?}`。
  - ★★ **返工＝新建一个生产批（新批号，独立取号）+ `b_rework(new_batch_id, src_batch_id, reason)`** —— ★ **不得**用「原批 + 后缀」（`UC-M6-05` 明文）。
  - `reason` **必填非空**；三项物料 / 客户**缺省继承原批**（显式覆盖则照常校验）。
  - ★ **新批的链根日期 = 新批创建日**（⇒ 新批码的日期段是**当日**，不是原批日期）—— 易错点，见 §8。
  - ★ 前置：`src_batch` 存在且 `status <> '已作废'`。★ **不强制**原批存在「不合格成品」（放宽读法，理由见 §6-16）。
- `GET /api/prod/rework?src_batch_id=`：查返工关联（读，`prod.rework` 的 `LevelRead`）。
- 权限：`prod.rework`，级别 **`LevelInit`**（★ **不得**用 `LevelAll`，否则 `production` 的 `INIT` 被挡；★ **不设审批入口**，返工**不在** `spec/permission-points.json#constraints.approval_scope` 的四类里）。

### D7 · 前端（Vue3）

- **生产批列表页**：建批 · 状态过滤 · 进入详情。
- **生产批详情页**：投料扫码（含拒绝原因回显）· 投料明细（可更正 / 删除，须填原因）· 作业段列表与新增 · 成品批列表与生成 · 成品袋批量生成与打印/补打 · 返工入口。
- **谱系查询**：按袋码反查去向（多对多如实显示）。
- ★ 动作按权限显隐（沿用批 4 / 批 5 形态：`GET /api/prod/perm-summary` 之类），**服务端仍是唯一权威**（前端隐藏 ≠ 服务端放行）。
- ★ 构建产物仍 `//go:embed` 进二进制（`scripts/build.sh` 形态不变）。

### D8 · 门禁保持

- ★ `bash scripts/check_all.sh` **必绿 11 项不得回退**（+ 2 会报项不得出现新命中）。
- ★ `check_perm_registry.py` **必须仍绿** —— 本批新增 **6 个 `prod.*` 权限点的消费端**（每点至少被一处 `RequirePerm(permission.ProdXxx, …)` 消费）。
- ★ 本批**不得新增 / 删除任何权限点**（51 × 6 = 306 固定）；**不得改** `spec/*.json` / `spec/schema.sql` / `docs/*`。

---

## 4. 验收判据（WorkBuddy 会逐条独立复核，★ 一律不采信回执）

| # | 判据 | 怎么验 |
|---|---|---|
| A1 | `bash scripts/check_all.sh` **必绿全绿** | 复跑（11/11） |
| A2 | ★★ **建生产批**：码形如 `1C-CG-0001-0003-261007-03`（物料段＝**计划产出成品物料**） | `TC-M6-01`：读库断言 `code` 逐字；★ 批序 scope ＝ 客户 + 成品物料 + **创建日** |
| A3 | ★★ **投料扫码写 `b_feed_record`** | `TC-M6-02`：扫 3 个吨袋码 ⇒ `b_feed_record` **3 行**且 `batch_id` 指对该批；★ 断言按**本批作用域**计数（★ 不得全库 `COUNT(*)`） |
| A4 | ★ **未出结论的袋投料 ⇒ 拒绝** | `TC-M6-03`：造「待检」车次 ⇒ 拒绝（400/409），且**库中无新增 feed_record** |
| A5 | 紧急放行的料投料 ⇒ **允许且留痕** | `TC-M6-04`：★ 须构造**生效**的紧急放行（init + approve 两笔、两人）⇒ 允许；★ **反例**：只有 `init` 一笔 ⇒ **仍拒** |
| A6 | ★★ **谱系多对多**（一车→多批、多车→一批） | `TC-M6-05`：A 车料投给批 1、批 2；B 车料也投给批 2 ⇒ 谱系表如实记录 **2×2** 关系（**不得**压成"批只挂一个袋"） |
| A7 | 作业段支持**跨班组** | `TC-M6-06`：一个批记 2 段（两个班组）⇒ **2 条**作业段，批仍为 **1** 个；`uk_op_seq` 生效 |
| A8 | ★ **成品批码序 1 ＝ 来源生产批序** | `TC-M6-07`：生产批 03 产出成品批 ⇒ 成品批码序 1 = `03`；★ **日期段 = 生产批 `batch_date`**（不是生成当日） |
| A9 | 成品袋批量生成 + 打印留痕 | 生成 N 袋 ⇒ N 行，`bag_seq` 1..N 且 `uk_fgbag_seq` 唯一；★ `b_label_print` 有行（补打 `is_reprint=1` + `reason` 非空） |
| A10 | **返工：新批号 + `b_rework` 关联原批** | `TC-M6-08`：新批 `id ≠ 原批`、`code` **独立取号**（不是原批加后缀）；`b_rework` 有 1 行且 `new_batch_id` / `src_batch_id` 正确；★ 新批码日期段 = **当日** |
| A11 | **M6 的 9 条 TC 均有自动化测试** | `TC-M6-01~09` 逐条对得上测试函数（测试名带 TC 编号，形如 `TestTC_M6_01_…`；沿用批 4 / 批 5 的 store + httpapi 双层形态） |
| A12 | 6 个 `prod.*` 点**真在路由层被消费** | `check_perm_registry.py` 绿；★ 并**读路由代码**确认不是只在 `all.go` 挂名；★ 级别逐个核对（`prod.rework` → **`LevelInit`**，其余写入 → `LevelAll`，读 → `LevelRead`） |
| A13 | ✓ **投料记录更正 / 删除留痕** | 读实现 + 用例：更正 / 删除**都必填 `reason`**，均写 `s_audit_log`（`action='correct'` / `'delete'`，带 `old_value`）；★ **改 `batch_id` / `bag_id` 的路径不存在** |
| A14 | ★ 取号：**并发串行 + 超限明确报错** | 读实现：`GET_LOCK` + **同事务**；★ 无"先查 max 再 +1"；超上限（批序 > 99 / 成品批序 > 999 / 成品袋序 > 999）**明确报错**，**不自动进位** |
| A15 | ★★ **联动修正**：退车排除已投料袋 | 造「车次有 1 袋已投料」⇒ `POST /api/recv/trucks/:id/return` **必须被拒**；★ **M3 既有全部 TC 保持绿**（见 §6-24） |
| A16 | ★★ **D23 边界**：实际产出只在 `b_fg_lot` | 读实现：全仓**无**对 `b_production_batch` 的产出字段写入（表里也没有）；产出物料 / 净重 / 袋数 / `produced_at` 只出现在 `b_fg_lot` |
| A17 | ★ 服务器真跑全套 TC 全绿 | `bash scripts/run_tc_server.sh`（★ 包集**自动发现**，输出会先打印「包集（N 个）」）；回执须写明**实际包数**与**各包 PASS/FAIL/SKIP** |
| A18 | ★ 服务真起且版本一致 | `bash scripts/deploy-test-server.sh --restart --smoke` ⇒ `/healthz` **200** 且 `version == HEAD`；★ 端口只绑回环 |

★ **单点变异自证（必做，≥2 处）**，例如：
- 去掉「投料前置的现行检测单结论校验」⇒ **A4 必须红**（且只有它对应用例红）；
- 把 `D` 的序 1 改成**成品批自己的流水**（而不是来源生产批序）⇒ **A8 必须红**；
- 把 `D`/`E` 的日期段改成**生成当日**⇒ **A8 / A10 必须红**；
- 把投料记录改成**无 `reason` 也放行**⇒ **A13 必须红**；
- 把 `ReturnTruck` 的已投料守卫去掉 ⇒ **A15 必须红**。

把变异点、红的证据（哪些用例红、哪些保持绿）、还原后的 `sha256` 写进回执。

---

## 5. 明确不做（本批）

| 项 | 为什么 |
|---|---|
| 出货（逐袋归集 / 出场登记 / 撤销） | **批 7**（M7） |
| **追溯页 / 批次档案页 / 正反向互为逆的完整断言** | **批 7**（M8）—— 本批只做 D5 的**数据可查**下限 |
| 对外报告页 / 报表 | **批 8**（M9 / M10） |
| **生产批 / 成品批 / 成品袋的「作废」入口** | ★ **权限点字典（51 点，冻结）无对应点** ⇒ **结构性不做**（不是偷懒）；如确需 ⇒ **开议题** |
| **生产批「标记完成」的状态推进** | ★ 同上：无对应权限点，且无 UC/TC 支撑 ⇒ **不做**；建批一律 `进行中` |
| 「整车/整批结论的质量加权平均 / AQL」 | **批 8**（M10 报表算法） |
| 留样到期提醒 · 仪器直连 | **二期**（`docs/03` §4） |
| 行级 / 列级权限 · 真实飞书回调 | 一期不做 / 沿用 dev 桩 |

★ 本批需要「车次 / 吨袋 / 车次检测单」等**上游对象**才能验投料与谱系 —— 允许用**测试夹具直写库**造最小对象（与批 3 / 批 4 / 批 5 同一手法）；★ **不得**为此改 M3 / M5 的实现（**唯一例外**见 §6-24 的联动修正）。

---

## 6. 口径要求（易错点，逐条确认）

1. ★ 表结构以 `spec/schema.sql` 为准 —— **列语义不得改**；要改 ⇒ **开议题**（判例见 `N-006` / `N-009`：**以机读件为准**，文档写错则改文档）。
2. ★★ **【硬约束一 · 环境】系统调试一律在测试服务器（`192.168.10.50`）上进行，不在本机（Windows）进行** —— 详见 **`docs/05-环境与调试约定.md`**。
   - ❌ 本机**不得**：跑服务（`go run` / 起监听）、连库做迁移或联调、起临时 MySQL、建隧道、重启服务验判据；
   - ✅ 本机**可以**：`go build`（只编译）、`go test`（★ **纯单元测试，不得依赖外部服务**）、`gofmt` / `go vet`、门禁脚本、只读探针；
   - ✅ 运行与联调一律走：`bash scripts/deploy-test-server.sh --restart --smoke`；批量用例走 `bash scripts/run_tc_server.sh`；
   - ★ **判据（一句话）**：**要 listen 或要改远端状态的，去服务器；只读、只编译、只静态检查的，留本机。**
3. ★★ **【硬约束二 · 提交】远端已接**：`origin` = `git@github.com:chadhao/jx-lab-trace.git`（分支 `main`）。★ **你只做本地提交，不 push** —— **推送由 WorkBuddy 在独立验收通过后执行**。提交用**显式路径**（**禁 `git add -A`**）。★ **并按铁律 12 分阶段提交**（见 §0 纪律 A）。
4. ★★ **生产批码 `C` 取号（我方定，不是草稿）**：`V=1` · `T=C` · `BT`（见下条）· 客户段＝客户编号 · **物料段＝`planned_output_material_id`（计划产出成品物料）的 4 位编号** · 日期段＝`batch_date`（`YYMMDD`，服务端当日，**链根日期**）· **序1＝批序**（scope ＝ **客户 + 成品物料 + 创建日**，宽 2，max **99**）· `序2=000` · `序3=000` · 校验位。★ 取号 = `GET_LOCK` + **同事务** + 前缀 `LIKE '1CCG<客户><物料><日期>%'` 取最大 +1；**超 99 明确报错、不自动进位**。★ 与批 3 `newNoticeNo` / 批 4 样品序号 / 批 5 `inspection_no` **同一手法**。
5. ★★ **`C`/`D`/`E` 的 `BT` 段固定 `CG`（我方定案，理由如下）**：本项目业务为**受托加工（客供）**，`b_production_batch.customer_id` 为 `NOT NULL` 且建批必填客户 ⇒ 与 `CG` 语义一致；★ **`b_production_batch` 表内无 `biz_type` 列**（`spec/schema.sql:296-310`，冻结件）⇒ 自购成品链（`ZG`）**在当前 schema 下无法表达**。★ 若你认为必须支持 `ZG` 成品链 ⇒ **开议题**，**不得**自改 schema 或自造字段。
6. ★★ **物料段查询不得直接复用 `lookupCodesCtx`** —— 它**硬编码 `kind='原料'`**（`internal/store/receiving.go:858`）；M6 的物料是 **`kind='成品'`**。⇒ 另写/参数化一个按 `kind='成品'` 校验的查询，**沿用同样的校验口径**（`is_current=1` 且 `status='启用'`，否则拒绝）。
7. ★★ **`D`/`E` 的「客户段 + 物料段」必须与来源生产批一致**：`b_fg_lot.customer_id` 必须 = `b_production_batch.customer_id`；`b_fg_lot.output_material_id` 必须 = `b_production_batch.planned_output_material_id`。★ 理由：`docs/02` §2 要求序段层层引用，前提是**同链物料段 / 客户段一致**（`docs/01` D11 原文「生产批按产出成品物料编号 ⇒ 同链物料段一致」）。**不一致 ⇒ 拒绝**（400）。
8. ★★ **`D`/`E` 的日期段 = 来源生产批的 `batch_date`**（**链根日期**，`docs/02` §4-4：「日期段取链根日期，跨零点不重编」）。★ **不得**用成品批生成日 / `produced_at` / 当日。
9. ★★ **三层序的取法（逐字，`spec/code-rules.json#object_types`）**：
   - `C`：`seq1 = 批序`、`seq2 = 000`、`seq3 = 000`；
   - `D`：**`seq1 = 来源生产批的批序`**、`seq2 = 该生产批内第 n 个成品批`（1 起，max 999）、`seq3 = 000`；
   - `E`：**`seq1 = 来源生产批序`**、`seq2 = 成品批序`、`seq3 = 本成品批内第 n 袋`（1 起，max 999）。
   ★ 序段表达的是「**从链根到本对象**的完整路径」，**不是本对象自己的流水**。
10. ★★ **`b_production_batch` 与 `b_fg_lot` 的边界（D23，本批红线）**：
    - **实际产出只在 `b_fg_lot` 记一次** —— 产出物料 / 包装规格 / 袋数 / 净重 / 产出时间；
    - `b_production_batch` **只留「计划产出物料」**（用于批号编排与排产），**不得**写任何产出数据；
    - `b_batch_operation.output_weight` 是「**本作业段**产出（吨），非整批产出」（列注释原文），**不得**当作整批产出。
11. ★★ **投料前置（三项，缺一即拒；我方定案）**：
    1. **袋可用**：`b_bag.status='在库'` 且该 `bag_id` **无既有 `b_feed_record`**（★ 一袋只投一次，见下条）；
    2. **已出结论**：该袋所属**车次**的**现行检测单** `conclusion ∈ {合格, CONCESSION}` —— ★ **「现行单」判定必须复用 M5**（`internal/store/inspection.go` 的 `currentInspectionIDTx`，必要时提取为可导出 helper），★ 已作废单（`b_obj_void`）**不算**；
    3. **或已获紧急放行**：该车次存在**生效的**紧急放行（见下条）。
    ⇒ `不合格` / 无结论（`待检`）/ 尚未取样，**且**无生效紧急放行 ⇒ **拒绝**（409，提示「该车尚未出结论，不得投料」）。★ 依据：`docs/01` §6.4「**默认『待检禁用』：未出结论的原料不得投料**」＋ `UC-M6-02`。
12. ★★ **紧急放行「是否生效」的判定（跨模块口径，我方定案）**：只认 **`s_audit_log`** —— 同一 (`entity='b_truck_lot'`, `entity_id`) 上**同时存在** `action='urgent_release_init'` **与** `action='urgent_release_approve'` 两笔，**且两笔 `actor_open_id` 不同** ⇒ **生效**。★ **只有 `init` 一笔（未经审批）⇒ 不生效**；★ **不得**只看「有没有调用过放行接口」、也不得只看权限层（依据：M5 的 `§6-11` 双保障与 `approval_scope`）。★ 生效后按 §6-11 第 3 条放行投料，**照常写 `b_feed_record`**（留痕）。
13. ★★ **一袋只投一次（并发安全）**：靠 **`b_bag.status` 守卫 + `b_feed_record` 存在性双向检查**（`SELECT ... FOR UPDATE` 锁袋行）；投料成功后同事务把袋置 **`已投料`**。★ **不新增唯一索引**（`spec/schema.sql` 冻结；`b_feed_record` 只有普通索引 `idx_feed_batch` / `idx_feed_bag`）—— 若你认为必须有 DB 级唯一约束 ⇒ **开议题**。★ **谱系的「多对多」在「车次 × 生产批」维度天然成立**：同一车的**不同袋**可分投不同批、不同车的袋可投同一批 —— `TC-M6-05` 的 2×2 就是这么来的；★ **不得**把它压成"一批只挂一个袋"或"一车只挂一批"。
14. ★ **枚举取值一律用 `spec/schema.sql` 列注释里的中文原词**：生产批 `status ∈ 进行中/已完成/已作废` · 成品批 `status ∈ 在库/已出货/已作废` · 成品袋 `status ∈ 在库/已出厂/作废` · 袋 `status ∈ 在库/已投料/已退回/留样中/作废` · `B`/`C`/`D`/`E` 的 `T` 段逐字为 `A`/`B`/`C`/`D`/`E`。★ **不得自造英文码**（与 M3 的 `disposition='退货'`、M4 的 `role='份样'`、M5 的 `state='未测'` 同一口径）。
15. ★★ **投料记录更正 / 删除的口径（我方定案）**：`b_feed_record` **无 `version` 列** ⇒ 按 `spec/schema.sql` 头部自述的**机械保证**（「带版本链的表，其业务键唯一索引必须含 `version`」）它**不是版本链表**，故**不建版本链**，采用「**更正（改可变量）+ 必填原因 + 审计留痕**」与「**物理删除 + 必填原因 + 审计留痕**」：① **可变量只有 `feed_weight` / `remark`**；② `reason` **必填非空**；③ 审计 `action ∈ {correct, delete}`，`old_value` 记快照；④ ★ **`batch_id` / `bag_id` 不可改**（那是**谱系关系本身** —— 改它等于篡改谱系，绕过它等于允许"这张料其实去了别处"）；⑤ **删除**后同事务把该袋 `status` **回置 `在库`**。★ 若你认为**必须**版本链 ⇒ **开议题**（判例同 `N-009`），**不得**自改 schema。
16. ★ **返工的前置口径**：① 权限 `prod.rework`，入口守卫 **`access.LevelInit`**（`production` = `INIT`、`qc` = `ALL` **都过**；★ 用 `LevelAll` 会把 `production` 挡在门外）；② ★ **不设审批入口** —— 返工**不在** `constraints.approval_scope` 的四类审批事项（留样销毁 / 紧急放行 / 出货单撤销 / 让步双签）内，`INIT` 在此的语义是「**可发起**」；③ 前置 = 原批存在且 `status <> '已作废'`；★ **不强制**「原批存在不合格成品」（`UC-M6-05` 的前置写成「有不合格成品」，但实践中返工也可能因客户要求 / 内部质量决定发起；**不作硬前置**，改为**必填 `reason`** 留痕）—— 这是**放宽读法**，理由记此备查；④ 新批 `code` **独立取号**，**不得**原批加后缀。
17. ★ **权限级别的取用（逐点核对，易错）**：
    - 写入口 `prod.batch.create` / `prod.feed.scan` / `prod.feed.correct` / `prod.op.log` / `prod.fg.gen` ⇒ **`access.LevelAll`**；
    - `prod.rework` ⇒ **`access.LevelInit`**（**不是** `LevelAll`）；
    - 所有**读**入口（列表 / 详情 / 谱系 / 返工关联查询）⇒ **`access.LevelRead`**，并**挂到语义最近的点**（生产批 ⇒ `prod.batch.create`；投料与谱系 ⇒ `prod.feed.scan`；产出 ⇒ `prod.fg.gen`；作业段 ⇒ `prod.op.log`；返工 ⇒ `prod.rework`）。
    - ★ **不得**新增权限点、**不得**裸写权限点字符串（必须用 `internal/permission/code.go` 已声明的常量）。
18. ★ **`b_fg_lot.qty_bag` 是「实际生成袋数」**：生成成品袋后**同事务**更新；`weight_allocated` 的摊算沿用 M3 `GenerateBags` 的手法（**毫吨整数**做算术、half-up，避免二进制浮点误差）。
19. ★ **打印与补打复用 M3 的 `b_label_print`**：`code` = 被打印对象码（成品袋码）；**补打 `is_reprint=1` 且 `reason` 必填非空**（`docs/02` §6「否则会出现『一物两码』」）；★ 一期**不写打印驱动**，只出**标签版式数据**。
20. ★★ **写 `COLLAB.md` 只能用「局部追加」** —— **禁止整体重写，禁止用格式化工具把该文件整体写回**（已出事故：底本是旧版本 ⇒ 冲掉对方内容）。门禁 `scripts/check_collab_anchors.py` 会判红这类覆盖。
21. ★★ **判据纪律（附加铁律 9）**：凡「在某范围内找某标记」的判据，**必须匹配结构化位置，不得匹配裸词** —— 裸词会被文档里的「讨论」命中而**恒真**。判据自身必须先被探针打过（真阳性保留 / 假阳性消除 / 旧逻辑复现）。
22. ★★ **heredoc 纪律（附加铁律 10）**：`<<EOF` **不加引号**时，体内**反引号与未转义的 `$` 会在本机被执行/展开**。⇒ 要发给远端的脚本，**注释里一律用「」，不要用反引号**；需要远端展开的 `$` 写 `\$`。
23. ★ **不加 `cgo`、不加需要联网下载的重依赖**；如需新增依赖，**先在回执里说明理由**（批 3 引 `skip2/go-qrcode` 走的就是这个流程）。二维码 / 码解析**沿用批 3 的 `internal/codec`**，不得另写一套。
24. ★★ **联动修正（本批必做，改动最小）**：`internal/store/receiving_bag.go` 的 **`ReturnTruck`（第 536 行起）整批作废袋码时，必须拒绝「该车次下存在已投料袋」**。理由：该函数现有注释写「退车意味着整车退回，取样/投料的前置本身已不成立」—— ★ 这句在**引入 M6 投料之后不再成立**（料已进生产批，物理上不可能退回）；而 `VoidBag` 的「已取样 / 已投料 ⇒ 拒绝作废」守卫**在 `ReturnTruck` 里被整批 `UPDATE` 绕过了** ⇒ **必须补上前置**（建议：`SELECT COUNT(*) FROM b_feed_record f JOIN b_bag b ON b.id=f.bag_id WHERE b.truck_lot_id=?` > 0 ⇒ 拒绝，报错写明「该车已有 N 袋投料，不能退车」）。★ **M3 既有全部 TC 必须保持全绿**（既有用例不涉及投料，故不受影响）；★ 该改动写进回执（属**集成修正**，不是范围蔓延）。
25. ★ **测试夹具纪律**（批 3 踩过、批 4 / 批 5 沿用）：跨包共用测试库时，清理必须**按账号精确匹配**（禁用前缀通配）、**先删子表再删父表**；断言「某表为空」必须**限定本用例作用域**，不能全库 `COUNT(*)`。★ M6 子表多（`feed_record` / `batch_operation` / `fg_bag` 都指向父表），**先删子再删父**尤其重要。
26. ★ **M6 不得顺手实现 M7 / M8 的功能**（出货 / 追溯页 / 批次档案页）—— 本批只到 D5 的**数据可查**下限。
27. ★ **不允许物理 `DELETE` 除「投料记录」以外的任何业务行**（更正 / 返工**一律新开**，**不得**改历史单据的业务字段）。

---

## 7. 完成后（三条全满足）

1. `COLLAB.md` 中**本批议题段**写回执 + 状态改 `MIMO-DONE`（★ 必须是**行首恰为 `- **状态**：MIMO-DONE`** 的那种行 —— 驱动的判据① 只认这个结构化位置）；
2. **提交代码**（显式路径，**禁 `git add -A`**）；★ **按铁律 12 分阶段提交**（后端一块 / 服务器 TC 全绿 / 变异自证 / 前端，各提交一次）；
3. 提交前 `bash scripts/check_all.sh` **全绿**，并在服务器跑 `bash scripts/run_tc_server.sh`（★ 输出会先打印「包集（N 个）」—— 请把**实际包数**与各包结果写进回执）。

★ 若发现**本批过大**，**回执说明并建议拆分**（`COLLAB.md §2` 允许），**不要硬做完、也不要闷头做一半**。
★ 若发现**规格本身有问题**（`spec/` 或 `docs/04` 的 UC/TC 有矛盾），**开议题**，**不要自己改规格**。

---

## 8. 本批我方（WorkBuddy）已先行指出的易错点汇总

| # | 易错点 | 落在哪条 |
|---|---|---|
| 1 | 投料**不写 `b_feed_record`**（或另建表 / 把谱系编进码） | §3-D2 / §6-13 / A3 |
| 2 | 把「未出结论不得投料」判成**只看袋**（应看**袋所属车次**的现行检测单） | §6-11 / A4 |
| 3 | 紧急放行**只有 init 就放行**（应 init + approve 两笔且两人不同） | §6-12 / A5 |
| 4 | 把谱系压成「一批只挂一个袋」/「一车只挂一批」 | §6-13 / A6 |
| 5 | `D`/`E` 的**序 1 用了自己的流水**（应为**来源生产批序**） | §6-9 / A8 |
| 6 | `D`/`E` 的**日期段用了生成当日**（应为**生产批 `batch_date`**） | §6-8 / A8 / A10 |
| 7 | 把产出（物料 / 净重 / 袋数 / 时间）也写进 `b_production_batch` | §6-10 / A16 |
| 8 | 直接复用 `lookupCodesCtx` 查**成品**物料（它硬编码 `kind='原料'`） | §6-6 |
| 9 | 自购（`ZG`）成品链**自造字段 / 自改 schema** | §6-5 |
| 10 | 返工入口用 `LevelAll` ⇒ `production` 的 `INIT` 被挡 | §6-16 / §6-17 / A12 |
| 11 | 返工用「原批 + 后缀」而不是**独立取号的新批** | §6-16 / A10 |
| 12 | 投料记录更正**无 `reason`** / 允许改 `batch_id`、`bag_id` | §6-15 / A13 |
| 13 | 忘了**联动修正** `ReturnTruck`（已投料袋被整批作废） | §6-24 / A15 |
| 14 | 取号写成「先查 max 再 +1」（应 `GET_LOCK` + 同事务） | §6-4 / A14 |
| 15 | 为「凑验收」自造**作废入口 / 生产批完成入口**（无权限点，结构性不做） | §5 |
