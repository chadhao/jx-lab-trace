# MIMO-NEXT-BATCH-07 · 批 7：M7 出货 + M8 追溯

> **交付方**：mimo code ｜ **验收方**：WorkBuddy｜ **派出日**：2026-10-09
> **批次**：批 7 / 8 ｜ **模块**：**M7 出货 ＋ M8 追溯** ｜ **依赖**：批 1（M0 地基）＋ 批 2（M1 主数据 ＋ M2 权限配置页）＋ 批 3（M3 收货与打码）＋ 批 4（M4 取样与留样）＋ 批 5（M5 检测）＋ 批 6（M6 生产与谱系）—— **六者均已独立验收通过 `AGREED`**（`N-001` / `N-007` / `N-010` / `N-011` / `N-012` / `N-013`）
> ★ **开工前必须完整读 `MIMO-ONBOARDING.md`**（含铁律与禁止事项），再读本文件。

---

## 0. 本轮说明（先读这段再读 §1）

> ★★ **2026-10-09 16:5x 更正：本轮是「续派」，不是从零开工。**
> 上一轮（14:20–15:43）**中途被回收**（驱动被整树终止，陈锁未释放）。**已完成并本地提交**：**D1–D6 后端 = `19b0f91`**（`internal/httpapi/server.go` · `internal/httpapi/ship.go` · `internal/httpapi/trace.go` · `internal/store/shipment.go` · `internal/store/trace.go`，5 文件 / **+1936 行**）—— ★ **仅本地、未 push**（`origin/main` 仍为 `a0b2d53`）。
> **未完成**：**D7（Vue3 前端）** 与 **D8（回执 ＋ 门禁保持）**；以及 D1–D6 的 **`internal/store` 层配套测试**（当时留下 `m7_test.go` / `m8_test.go` 两个**未写完且编译不过**的文件）。
> ★ 该两份 WIP 已由 WorkBuddy **归档**到 `.workbuddy/wip-archive/2026-10-09-1645-N-014/`（`m7_test.go.wip` · `m8_test.go.wip` ＋ `README.md`，**内含当时的完整编译错误清单**）—— **可参考其用例意图**，但**不必迁就**其写法（它编译不过）。
> **本轮基线**：`HEAD = 19b0f91`（本地）· 工作区**干净** · 门禁 **11/11 全绿**（WorkBuddy 于 2026-10-09 16:5x 实测）。
> ★★ **D1–D6 已完成、不要重做**；直接从 **D7** 起，并**补齐 M7 / M8 的 store 层与 httpapi 层测试**（D2 / D4 / D5 / D6 各条均要求双层覆盖）。★ 铁律 12（**不要用 `git stash` / `git checkout` 处理未提交的 WIP**）**继续有效**。

★ **请务必带上批 5 / 批 6 换来的两条纪律**（见 `COLLAB.md §0` 附加铁律 11 / 12）：

| # | 纪律 | 为什么 |
|---|---|---|
| **A** | **分阶段本地提交**：每完成一块（后端一块 / 服务器 TC 全绿 / 变异自证 / 前端）**立刻用显式路径提交一次**，**不要攒到最后一次性提交** | 派工轮随心跳 turn 结束被整树回收是**常态风险**（批 5 首次派工就撞上，25 分钟工作量零提交）；分批提交后**任何一次回收最多只丢最后一块**（批 5 续派 / 批 6 均按此拿到交付） |
| **B** | **不要用 `git stash` / `git checkout` 处理未提交的 WIP** | 本仓库 `core.autocrlf=true` 且无 `.gitattributes` ⇒ stash 落成 **CRLF** ⇒ `gofmt -l` 判红、门禁 1/11 失败（2026-10-09 实测踩中） |

★ **本条不改变**「只本地提交、不 push」（硬约束二）。

★ **本批是「一期业务闭环的最后一环」**：M7 出货 + M8 追溯做完，`收货 → 取样 → 检测 → 生产 → 出货 → 追溯` 全链打通；批 8 只剩「对外报告分享 + 报表」两个**对外/只读**模块。故本批**口径尤其重要** —— 追溯是「给人看的证据链」，**正反两个方向互相矛盾**会直接摧毁系统可信度。

---

## 1. 本批目标（一句话）

**把货发出去、并且事后能追回来**：逐袋扫成品袋码归集成出货单 → 出场登记 → （如有）撤销（发起 ≠ 审批）；以及**正向**（车次/吨袋 ⇒ 生产批 ⇒ 成品批 ⇒ 出货单）与**反向**（成品批 ⇒ 生产批 ⇒ 投料吨袋 ⇒ 车次 + 检测结果）**互为逆**的追溯，加一页**批次档案**（内部档案，标注让步接收使用）。

★ 本批**最重要的一条**（做错就要返工）：
1. ★★ **「同一成品袋不能被两个出货单归集」必须由应用层落实** —— `spec/schema.sql:566` 的 `uk_ship_bag(shipment_id, fg_bag_id)` **只保证「同一单内不重复」**，**不是跨单全局唯一**（见 §6-2）。★ 判据 = 袋行 `FOR UPDATE` + `b_fg_bag.status='在库'` **+ 「在未撤销的出货单中无该袋明细」** 三段合取。
2. ★★ **正反追溯必须走同一份数据（`b_feed_record`）**，且**正反互为逆**（`TC-M8-02`）—— 正向用的关联路径与反向用的必须是同一张表、同一组连接条件。
3. ★★ **撤销的「发起」不生效、「审批」才生效**，且 **发起人 ≠ 审批人**（`TC-M7-05`）—— 与批 6 紧急放行「只有 init 不生效」同一口径。
4. ★★ **M8 全模块只读**：追溯与档案**零写业务表**（`A17` 可机检）。

---

## 2. 必读

| # | 文件 | 读什么 |
|---|---|---|
| 1 | `COLLAB.md` | §0 铁律（★ 含**附加铁律 6~12**）· §1 当前状态 · §2 责任域 · §4 **`N-008` / `N-014`** |
| 2 | `docs/04-模块设计与用例.md` | ★★ **`M7` 的 UC-M7-01~03 / TC-M7-01~06** ＋ **`M8` 的 UC-M8-01~03 / TC-M8-01~05**（这是你的验收标准，共 **6 UC / 11 TC**） |
| 3 | `docs/03-模块划分与实施批次.md` | **批 7 的 4 条验收要点**（§2 表格「批 7」行） |
| 4 | `docs/01-设计定案.md` | **§5.5 出货（表清单）** · **§6.5 成品出场**（流程）· **§6.6 追溯与分享**（正反向链路原文）· **§11.2 D20**（让步接收：**对外不披露 / 内部档案显著标注**）· **D24**（权限点拆分规则：出货单撤销是 3 组「发起 / 审批」之一）· **P1**（数据不可变）· §8 权限矩阵「出货」「追溯」两行 |
| 5 | `docs/02-追踪码规则.md` | §2 / §3 / §6（★ 打印与补打）—— ★ 本批**不新增任何追踪码对象**：`shipment_no` **不是**追踪码（见 §6-1） |
| 6 | `spec/schema.sql` | 本批要动的 **2 张表**：**`b_shipment`(541)** `b_shipment_item`(559)；**只读**引用：`b_fg_bag`(363) `b_fg_lot`(344) `b_production_batch`(296) `b_feed_record`(329) `b_batch_operation`(312) `b_bag`(247) `b_truck_lot`(220) `b_inspection`(474) `b_obj_void`(277) `s_audit_log`(603) `m_customer`(27) `m_material`(83) `m_vehicle`(150) —— ★ **列注释里的枚举原词就是取值** |
| 7 | `spec/code-rules.json` | ★ 仅用于**解析**成品袋码（`T=E`）；★ **确认其中没有 `shipment` / `出货` 对象**（故 `shipment_no` 非追踪码，见 §6-1） |
| 8 | `spec/permission-points.json` | 51 个权限点（★ 本批消费其中的 **7 个**：4 个 `ship.*` ＋ 3 个 `trace.*`，**不得新增**）—— ★ 注意 `ship.void.approve` 的 levels 是 **`["APPROVE","NONE"]`**（**没有 `READ`**） |
| 9 | `internal/codec/codec.go` | ★ 码解析（`Parse` / `FromHuman` / `ObjectOf` / `ToHuman`）—— **必须复用**，不得另写 |
| 10 | `internal/store/production.go` | ★★ **本批最近的工程范式**：`normalizeBagCode` 码解析与拒绝（`FeedScan`）· 袋行 `FOR UPDATE` 守卫 · 同事务状态回置 · 取号（`acquireSeqLock` / `nextFgSeq`）· 审计快照 |
| 11 | `internal/store/inspection.go` | ★★ **追溯的「检测结果」要复用它的「现行检测单」判定**（`currentInspectionIDTx:169`）与紧急放行落痕（`UrgentReleaseInit:1073` / `UrgentReleaseApprove:1105`）—— **不得另写一套** |
| 12 | `internal/store/retention.go` | ★★ **「发起 / 审批」两段式的既有范式**（`InitDestroy:500` 只落待审批行、**不改状态**；`ApproveDestroy:575` 强制审批人非空）—— 本批撤销沿用同一形态，但**留痕落 `s_audit_log`**（因 `b_shipment` 无 void 列，见 §6-4） |
| 13 | `internal/store/m3_test.go` · `m4_test.go` · `m5_test.go` · `m6_test.go` | ★ 测试夹具纪律与双层形态（store / httpapi） |
| 14 | `docs/05-环境与调试约定.md` | ★★ **调试只在测试服务器**（见 §6 硬约束一） |

★ 7 个权限点**已在 `spec` 登记、常量已在 `internal/permission/code.go:57-63` 声明**（`ShipLoadScan` / `ShipOutRegister` / `ShipVoidInit` / `ShipVoidApprove` / `TraceForward` / `TraceBackward` / `TraceBatchView`），你只需**消费**（路由层引用），**不要**改声明文件或新增点。

---

## 3. 交付物

### D1 · 装车归集（`ship.load.scan`）—— ★★ 建单 ＋ 逐袋扫码

- `POST /api/ship/shipments`：body `{customer_id?, bag_codes: ["…"], remark?}` —— ★ **建单 ＋ 批量归集在同一事务**（`bag_codes` **至少 1 个**，空 ⇒ 400）。
  - ★ **出货单号 `shipment_no`（我方定案，见 §6-1）**：**`CH` ＋ `YYMMDD` ＋ `-` ＋ 当日 3 位序号**，形如 `CH261007-001`；取号手法 = **`GET_LOCK`（命名锁）＋ 同一事务**内按前缀取最大 ＋1（**禁止"先查最大值再 +1"**）；**超 999 明确报错**（`ErrShipSeqOverflow`），**不自动进位**。★ 与 M3 `newNoticeNo`（`YB…`）· M5 `nextInspectionNo`（`JC…-NNN`）**同一手法**。
  - `customer_id`：★ **必须与所有袋所属成品批的客户一致**（见 §6-5）；`customer_id` 缺省时由**首个袋**的 `b_fg_lot.customer_id` 推定。
  - `status` 建单即 **`已出厂`**（★ 冻结枚举只有 `已出厂` / `已撤销`；「是否已登记出场」用 **`ship_at IS NULL`** 表达，见 §6-6）。
- `POST /api/ship/shipments/:id/items`：body `{bag_code}` —— **向同一张单追加扫码**（★ 扫码枪逐袋场景）；同一套校验。
- ★★ **每袋归集校验（5 项，缺一即拒；见 §6-2 / §6-3）**：
  1. 码可解析且 **`T='E'`（成品袋码）** —— 非 `E` 类码 ⇒ 400；
  2. 袋存在，且 `b_fg_bag.status='在库'`（★ 袋行 **`SELECT … FOR UPDATE`**）；
  3. ★★ **该袋在「未撤销」（`status<>'已撤销'`）的出货单中不存在明细** ⇒ 否则 **409**「该成品袋已被出货单 `CH…` 归集」；
  4. ★ **跨客户**：该袋所属 `b_fg_lot.customer_id` ≠ 本单 `customer_id` ⇒ **400**（提示「跨客户装车须拆单」）；
  5. 本单 `status='已出厂'`（已撤销的单不得再扫码）。
- `GET /api/ship/shipments`（列表，可按 `?status=` / `?customer_id=` 过滤）/ `GET /api/ship/shipments/:id`（详情，含明细袋码）：读，级别 **`LevelRead`**。
- 权限：写入口 `ship.load.scan`，级别 **`LevelAll`**。

### D2 · 出场登记（`ship.out.register`）

- `POST /api/ship/shipments/:id/depart`：body `{vehicle_id?, plate_no, driver, ship_at?, operator?}` —— ★ **`plate_no` / `driver` 必填非空**（`UC-M7-02`：记客户/车牌/司机/时间）。
- ★ 前置：单 `status='已出厂'`；★ **至少 1 条明细**（空单不得出场登记 ⇒ 409）；★ **未登记过**（`ship_at IS NOT NULL` ⇒ 重复登记 **409**，不覆盖）。
- 效果（同事务）：UPDATE `b_shipment` 的 `vehicle_id` / `plate_no` / `driver` / `ship_at`（缺省 = 服务端当前时刻）/ `operator`；★ **同事务**把该单**所有明细袋** `b_fg_bag.status` 置 **`已出厂`**（`UC-M7-02` 后置）。
- ★★ **本批不动 `b_fg_lot.status`**（见 §6-7）：出货状态**以成品袋为准**；`b_fg_lot.status='已出货'` 的语义（「部分出货」如何表达）**未定义** ⇒ 一期**不写**该值。★ 若你认为必须写 ⇒ **开议题**，**不得**自行决定。
- 权限：`ship.out.register`，级别 **`LevelAll`**；读入口 **`LevelRead`**。

### D3 · 出货单撤销（发起 / 审批）—— `ship.void.init` ＋ `ship.void.approve`

> ★★ `b_shipment`（`spec/schema.sql:541-557`）**没有** `void_by` / `void_reason` / `approved_by` 等列（**冻结件**）⇒ 撤销的**发起 / 审批留痕一律落 `s_audit_log`**（与批 5 紧急放行同一手法，见 §6-4）。★ **不得**自加列、不得自建表。

- `POST /api/ship/shipments/:id/void`（**发起**）：body `{reason}` —— `reason` **必填非空**。
  - 权限 `ship.void.init`，级别 **`LevelInit`**（★ **不得**用 `LevelAll`，否则 `receiver` 的 `INIT` 被挡）。
  - ★★ **不改任何状态、不动袋**（与批 6 紧急放行「发起不生效」口径一致）。
  - 写审计：`entity='b_shipment'` · `entity_id` · `action='ship_void_init'` · `reason` · actor。
  - ★ 幂等：同一单**已有 init 未审批** ⇒ 再发起 **409**（明确报错，不覆盖）。
- `POST /api/ship/shipments/:id/void/approve`（**审批**）：权限 `ship.void.approve`，级别 **`LevelApprove`**。
  - ★★ **前置**：① 该单存在 `action='ship_void_init'` 审计（否则 ⇒ `ErrShipVoidNotInit` ⇒ 409）；② ★★ **审批人 ≠ 发起人**（比对 `s_audit_log.actor_open_id`）⇒ 相同 ⇒ **拒绝**（`ErrShipVoidSelfApprove`，403）—— **`TC-M7-05` 的落点**。
  - 效果（**同事务**）：`b_shipment.status='已撤销'`；★ **该单所有明细袋** `b_fg_bag.status` **回退 `在库`**（★ 仅当袋当前为 **`已出厂`** 时回退；袋为 `作废` 时**不动**）；写审计 `action='ship_void_approve'`。
  - ★★ **`b_shipment_item` 明细行保留**（历史留痕，**不物理删除**）；撤销后这些行**不再阻止**该袋进入新单（因 D1 第 3 项只查 `status<>'已撤销'` 的单）⇒ **`TC-M7-06` 成立**（撤销后袋可再出货）。
- `GET /api/ship/shipments/:id/void-records`：返回该单的发起 / 审批留痕（读，用 `ship.void.approve` 的 `LevelRead`？★ **不行** —— 该点 levels 无 `READ`）⇒ ★ **改用 `trace.batch.view` 的 `LevelRead`** 或 `ship.void.init` 的 `LevelRead`（**择优取有 `READ` 的点**，见 §6-8）。

### D4 · M8 正向追溯（`trace.forward`）

- `GET /api/trace/forward?truck_lot_id=` 或 `?bag_code=`（二者至少一个；★ 用 `codec` 解析袋码）：
  返回 `{source:{…}, batches:[{batch_id, code, batch_date, status}], fg_lots:[{fg_lot_id, code, output_material, net_weight, status}], shipments:[{shipment_id, shipment_no, status, ship_at, plate_no, customer}], has_flow:true|false}`。
- ★★ **链路必须走 `b_feed_record`**（`docs/01` §6.6 原文：**车次 → 吨袋 → 投料记录 → 生产批 → 成品批 → 出货单**）：`b_bag.truck_lot_id` → `b_feed_record.bag_id` → `b_production_batch` → `b_fg_lot` → `b_fg_bag` → `b_shipment_item` → `b_shipment`。
- ★★ **未被投料的车 / 袋 ⇒ HTTP 200 ＋ `has_flow:false` ＋ 三个数组为空**（**不是 404、不是 500**）—— `TC-M8-03` 明文「**明确显示"无流向"，不是报错**」。
- 权限 `trace.forward`，级别 **`LevelRead`**（★ 该点 levels 含 `ALL`，但这是**只读**入口）。

### D5 · M8 反向追溯（`trace.backward`）

- `GET /api/trace/backward?fg_lot_id=` 或 `?fg_code=`：
  返回 `{fg_lot:{…}, batches:[…], feeds:[{bag_code, truck_lot_id, truck_code, customer, feed_weight, fed_at, inspection:{…}}], has_flow:true|false}`。
- ★★ **每个投料吨袋必须带上「其车次的检测结果」**（`docs/01` §6.6：**车次（含当时检测结果）**；`TC-M8-04`：**检测结论与数值一并列出**）。
  - ★★ **「当时」的口径（我方定案 + 如实声明的边界，见 §6-9）**：`b_feed_record` **无检测单外键** ⇒ 数据库里**无法复原「投料那一刻的现行单快照」**。⇒ 实现返回该车次的**现行检测单**（★ **复用 M5 `currentInspectionIDTx`**，`b_obj_void` 已作废单**不算**），并在返回体里**显式标注** `inspection.is_current=true`（即「现行单」而非历史快照）；若该车次是以**生效的紧急放行**投料（见 §6-10）⇒ 额外标 `urgent_release=true` 并附 init / approve 两笔留痕。★ **不得**假装它是历史快照、**不得**为此改 schema。
- ★★ **一致性（`TC-M8-02`：正反互为逆）**：反向给出的每条 `batch_id` / `truck_lot_id`，必须与**正向**（D4）从该车次出发得到的集合**互为逆** —— ★ **两向必须走同一张 `b_feed_record`**，不得正向走投料表、反向走别的关联（如 `b_fg_lot.batch_id` 直连）⇒ 否则必出现「查得到 A 却回不到 B」。
- 权限 `trace.backward`，级别 **`LevelRead`**。

### D6 · M8 批次档案（`trace.batch.view`）

- `GET /api/trace/batch/:batchId`：**一页汇总该生产批全链**（内部档案）——
  批基本信息（码 / 客户 / 物料 / 链根日期 / 状态）＋ 投料明细（袋码 / 车次 / 投料量 / 时间 / 操作人）＋ 作业段（段序 / 班组 / 时段 / 本段产出）＋ 成品批与成品袋 ＋ 出货单（含撤销状态）。
- ★★ **让步接收标注**（`UC-M8-03` ＋ `TC-M8-05` ＋ `D20`）：
  - 判据（我方定案，见 §6-11）= 该批投料链上任一袋**所属车次的现行检测单 `conclusion='CONCESSION'`** ⇒ 返回体加 **`concession_used:true`** ＋ `concession_sources:[{truck_lot_id, truck_code, inspection_no, conclusion}]`；
  - ★ **内部档案显著标注**（页面顶部醒目提示）；★ **对外报告不披露**属 **M9**（`D20`），**本批不做**。
- 权限 `trace.batch.view`，级别 **`LevelRead`**。

### D7 · 前端（Vue3）

- **出货页**：建单归集（扫码枪逐袋回车 → 列表收集 → 提交；或逐袋追加）· 出货单列表与详情（明细袋码 / 状态 / 出场信息）· 出场登记表单 · **撤销**（发起 / 审批两个动作，按权限显隐，各带原因）。
- **追溯页**：正向查询（车次号 / 吨袋码）· 反向查询（成品批码 / 成品袋码）· **批次档案页**（含让步接收醒目标注）。
- ★ 动作按权限显隐（沿用批 4 / 批 5 / 批 6 形态：`GET /api/ship/perm-summary` 或同类），**服务端仍是唯一权威**（前端隐藏 ≠ 服务端放行）。
- ★ 构建产物仍 `//go:embed` 进二进制（`scripts/build.sh` 形态不变）。

### D8 · 门禁保持

- ★ `bash scripts/check_all.sh` **必绿 11 项不得回退**（＋ 2 会报项不得出现新命中）。
- ★ `check_perm_registry.py` **必须仍绿** —— 本批新增 **7 个权限点的消费端**（4 个 `ship.*` ＋ 3 个 `trace.*`）。
- ★ 本批**不得新增 / 删除任何权限点**（51 × 6 = 306 固定）；**不得改** `spec/*.json` / `spec/schema.sql` / `docs/*`。
- ★ **M8 三个模块零写业务表**（只读）；★ 全批**除撤销状态推进与出场登记外，不改任何业务行**。

---

## 4. 验收判据（WorkBuddy 会逐条独立复核，★ 一律不采信回执）

| # | 判据 | 怎么验 |
|---|---|---|
| A1 | `bash scripts/check_all.sh` **必绿全绿** | 复跑（11/11） |
| A2 | ★★ **逐袋归集**：扫 15 个成品袋码 ⇒ 一张出货单、明细 **15 行** | `TC-M7-01`：读库断言明细数、单号形态 `CH` ＋ `YYMMDD` ＋ `-` ＋ 3 位序号；★ 断言**按本单作用域**计数（不得全库 `COUNT(*)`） |
| A3 | ★★ **同一袋进第二张单 ⇒ 拒绝**；★ **撤销后可再归集** | `TC-M7-02` ＋ `TC-M7-06`：① 扫入单 A 后扫入单 B ⇒ **拒**（409）；② **撤销**单 A 后再扫入单 B ⇒ **允许**（★ 两向都验，缺一不可） |
| A4 | ★ 跨成品批同车允许；★ **跨客户必须拆单** | `TC-M7-03`：明细来自两个 `fg_lot` ⇒ 允许；★ 另验「同单混两个客户」⇒ **拒**（400，§6-5） |
| A5 | **出场登记 ⇒ 袋转「已出厂」**且车辆信息落库 | `TC-M7-04`：读库断言袋 `status='已出厂'`、`ship_at` 非空、`plate_no` / `driver` 落库；★ 重复登记 ⇒ 409 |
| A6 | ★ **撤销发起不生效 / 审批才生效** | 发起后读库断言 `b_shipment.status` **仍为 `已出厂`**、袋**仍 `已出厂``；审批后断言 `已撤销` ＋ 袋回退 `在库`（`TC-M7-06` 前置） |
| A7 | ★★ **撤销发起人自审 ⇒ 拒绝** | `TC-M7-05`：同一 actor 先 init 再 approve ⇒ **拒**（403/409），且**状态未变** |
| A8 | ★★ **正向追溯**：A 车 ⇒ 生产批 ＋ 成品批 ＋ 出货单 | `TC-M8-01`：读接口返回体，断言批 / 成品批 / 出货单齐；★ 链路**经 `b_feed_record`**（读实现核对） |
| A9 | ★★ **正反互为逆** | `TC-M8-02`：正向结果中**任一**成品批反向 ⇒ **能追回 A 车**（测试内做 A→B→A 回环断言） |
| A10 | ★ **未投料的车 ⇒ 200 ＋ 空 ＋ `has_flow:false`** | `TC-M8-03`：断言 **HTTP 200**（非 404 / 500）且数组为空、标志为 false |
| A11 | ★ 反向追溯**含检测结果** | `TC-M8-04`：断言 `inspection` 含 `conclusion` ＋ 数值（`b_inspection_result`）＋ 单号；★ 且 `is_current` 标注存在（§6-9） |
| A12 | ★ **让步接收标注** | `TC-M8-05`：造 `conclusion='CONCESSION'` 的车次料投入某批 ⇒ 该批档案 `concession_used=true` 且列出车次 |
| A13 | 批次档案**一页汇总全链** | 读接口返回体：批 / 投料 / 作业段 / 成品批 / 袋 / 出货单六块齐 |
| A14 | 7 个点**真在路由层被消费** ＋ 级别正确 | `check_perm_registry.py` 绿；★ 并**读路由代码**确认不是只在 `all.go` 挂名；★ 级别逐个核对：`ship.void.init` ⇒ **`LevelInit`**、`ship.void.approve` ⇒ **`LevelApprove`**、其余写入口 ⇒ `LevelAll`、**全部读入口 ⇒ `LevelRead`** |
| A15 | ★ 取号：**并发串行 ＋ 超限明确报错** | 读实现：`GET_LOCK` ＋ **同事务**；★ 无「先查 max 再 +1」；`shipment_no` 当日序号 > 999 ⇒ **明确报错**，**不自动进位** |
| A16 | **M7 / M8 的 11 条 TC 均有自动化测试** | `TC-M7-01~06` ＋ `TC-M8-01~05` 逐条对得上测试函数（测试名带 TC 编号，形如 `TestTC_M7_01_…`；沿用批 4 / 批 5 / 批 6 的 **store ＋ httpapi 双层**形态） |
| A17 | ★★ **M8 零写业务表**（只读模块） | 机检 ＋ 读代码：`trace` 相关代码**无对任何业务表的 INSERT / UPDATE / DELETE**（★ 用**结构化**方式核，别只 grep 裸词 —— 见 §6-14） |
| A18 | ★ 服务器真跑全套 TC 全绿 | `bash scripts/run_tc_server.sh`（★ 包集**自动发现**，输出会先打印「包集（N 个）」）；回执须写明**实际包数**与**各包 PASS/FAIL/SKIP** |
| A19 | ★ 服务真起且版本一致 | `bash scripts/deploy-test-server.sh --restart --smoke` ⇒ `/healthz` **200** 且 `version == HEAD`；★ 端口只绑回环 |
| A20 | **不动冻结件** | `git diff --stat <基线>..HEAD` 中 `spec/` · `docs/` · `migrations/` **改动文件数 = 0**；权限点仍 **51 × 6 = 306** |

★ **单点变异自证（必做，≥2 处）**，例如：
- 去掉 D1 第 3 项「未撤销单中无该袋」检查 ⇒ **A3 必须红**（且只有它对应用例红）；
- 撤销审批**不校验「发起人 ≠ 审批人」** ⇒ **A7 必须红**；
- 正向追溯**改走 `b_fg_lot.batch_id` 直连**（绕开 `b_feed_record`）⇒ **A8 / A9 必须红**；
- 正向追溯把 `has_flow` 写成**恒 `true`** ⇒ **A10 必须红**；
- 撮合让步判据**去掉 `CONCESSION`** ⇒ **A12 必须红**。

把变异点、红的证据（哪些用例红、哪些保持绿）、还原后的 `sha256` 写进回执。

---

## 5. 明确不做（本批）

| 项 | 为什么 |
|---|---|
| 对外报告页 / 分享链接 / 访问日志 | **批 8**（M9）—— ★ 含 `D20`「对外不披露让步」的落点 |
| 报表（质量趋势 / 客户对账 / 产量合格率 / 留样到期 / 不合格统计） | **批 8**（M10） |
| **`b_fg_lot.status='已出货'` 的写入** | ★ **语义未定义**（「部分出货」如何表达？）⇒ 一期**在成品袋维度表达出货状态**（§6-7）；如确需 ⇒ **开议题** |
| **出货单的「物理删除」** | ★ 无权限点、无 UC 支撑 ⇒ **不做**；撤销即终态（`已撤销`） |
| 出货单**装车重量 / 运费 / 磅单**等扩展字段 | ★ `b_shipment` 无对应列（冻结件）＋ 无 UC 支撑 ⇒ **不做** |
| 追溯的**导出 / 打印 / 二维码** | 无 UC 支撑 ⇒ **不做** |
| 「整车/整批结论的质量加权平均 / AQL」 | **批 8**（M10 报表算法） |
| 行级 / 列级权限 · 真实飞书回调 | 一期不做 / 沿用 dev 桩 |

★ 本批需要「成品批 / 成品袋 / 车次 / 检测单 / 投料记录」等**上游对象**才能验出货与追溯 —— 允许用**测试夹具直写库**造最小对象（与批 3 / 批 4 / 批 5 / 批 6 同一手法）；★ **不得**为此改 M3～M6 的实现（**本批无联动修正**；若你认为确需 ⇒ 回执说明并**开议题**）。

---

## 6. 口径要求（易错点，逐条确认）

1. ★★ **`shipment_no` 不是追踪码**（我方定案）：`spec/code-rules.json` 中**无 `shipment` / `出货` 对象**（**本批开工前已核**）⇒ 出货单号**不适用 27 位追踪码规则**，也不编入任何追踪码。⇒ 取号 = **`CH` ＋ `YYMMDD` ＋ `-` ＋ 当日 3 位序号**（例 `CH261007-001`），`GET_LOCK` ＋ 同事务，**max 999 明确报错**。★ 与 M5 的 `JC…-NNN`（`docs/01` 已定「检测单号」形态）**同族**；★ `b_shipment.shipment_no` 为 `VARCHAR(40) NOT NULL` ＋ `uk_shipment_no` 唯一。
2. ★★ **「同一成品袋不能被两个出货单归集」必须由应用层落实**（**本批最高优先判据**）：★ `spec/schema.sql:566` 的 `uk_ship_bag (shipment_id, fg_bag_id)` **仅保证「同一出货单内同一袋不重复」**，**不是跨单全局唯一**（`b_shipment_item` 只有 `idx_shipitem_bag` 这一普通索引指向 `fg_bag_id`）。⇒ 判据 = **三段合取**：① 袋行 `SELECT … FOR UPDATE`（并发安全）；② `b_fg_bag.status='在库'`；③ **`NOT EXISTS`（该袋在 `status<>'已撤销'` 的出货单中存在明细）**。★ **不得**为此改 schema（硬约束）；若你认为必须 DB 级全局唯一索引 ⇒ **开议题**（判例同 `N-009`）。
3. ★ **袋的 `status` 只在本批两个动作上变**：① **出场登记** ⇒ `已出厂`；② **撤销审批** ⇒ 回退 `在库`。★ **归集本身不动袋状态**（归集的占用由第 2 条的「跨单存在性检查」表达）—— ★ 这是刻意的：冻结枚举只有 `在库 / 已出厂 / 作废`，**没有「已归集」**，而"归集但未出场"必须与"可再出货"区分 ⇒ **用明细行 ＋ 单状态表达**，不引入新枚举。
4. ★★ **撤销的发起 / 审批留痕落 `s_audit_log`**（我方定案）：`b_shipment` **无 void 相关列**（`spec/schema.sql:541-557`，冻结件）⇒ ① 发起：`action='ship_void_init'`（`reason` 必填），**不改状态**；② 审批：`action='ship_void_approve'`，随后置单 `已撤销` ＋ 袋回退。★ 判「是否已发起」＝ 查 `s_audit_log` 上该 `entity_id` 的 `ship_void_init` 行；★ 判「是否自审」＝ 比对两笔的 `actor_open_id`。★ **不得**自加列 / 自建表（同批 5 紧急放行、批 6 投料更正口径）。
5. ★★ **一张出货单只能一个客户**（我方定案）：`b_shipment.customer_id` 为 **`NOT NULL` 单值**（冻结件）⇒ 一单**只能同客户**；★ **跨成品批可以**（`TC-M7-03` 明确允许），★ **跨客户必须拆单**（否则 **400**）。理由：`b_shipment` 的数据模型本身就表达「一张单对应一个客户」；把它做成"混客户单"会让后续客户对账（M10）无解。
6. ★ **建单即 `已出厂`，「是否出场登记过」用 `ship_at IS NULL` 表达**（我方定案）：`b_shipment.status` 的**冻结枚举只有 `已出厂` / `已撤销`**（列注释原文），**且 `DEFAULT '已出厂'`** ⇒ ① 归集建单时即写 `已出厂`（沿用库默认）；② 「尚未出场登记」由 **`ship_at IS NULL`** 表达；③ 出场登记填 `ship_at` ＋ 车辆信息；④ 撤销 ⇒ `已撤销`。★ **不得**自造 `待出场` 之类新枚举（与 M3 `disposition='退货'`、M4 `role='份样'`、M5 `state='未测'`、M6 `status='进行中'` 同一口径：**枚举一律用冻结件列注释里的中文原词**）。
7. ★★ **本批不写 `b_fg_lot.status='已出货'`**（我方定案，见 §5）：出货状态**以成品袋（`b_fg_bag.status`）为准**。理由：一张出货单可跨多个成品批（`TC-M7-03`），一个成品批也可**部分出货**（只装走部分袋）⇒ 用批级 `status` 表达"是否已出货"必然产生**两份真相**（与 `D23` 同类问题）。★ 若日后确需批级出货状态 ⇒ **开议题**定义"部分出货"。
8. ★★ **读入口的权限点归属（逐点核对，易错）**：
   - 写入口：`ship.load.scan` / `ship.out.register` ⇒ **`access.LevelAll`**；`ship.void.init` ⇒ **`access.LevelInit`**；`ship.void.approve` ⇒ **`access.LevelApprove`**。
   - ★★ **所有读入口 ⇒ `access.LevelRead`**，且**只能挂到 levels 里含 `READ` 的点**：★ **`ship.void.approve` 的 levels 是 `["APPROVE","NONE"]`（无 `READ`）** ⇒ **不得**用它做读入口（否则所有人都读不到）。
   - 建议归属：出货单列表 / 详情读 ⇒ `ship.load.scan`；撤销留痕查询 ⇒ `ship.void.init`（levels 含 `READ`）；正向 / 反向追溯 ⇒ `trace.forward` / `trace.backward`；批次档案 ⇒ `trace.batch.view`。
   - ★ **不得**新增权限点、**不得**裸写权限点字符串（必须用 `internal/permission/code.go:57-63` 已声明的常量）。
9. ★★ **反向追溯的「当时检测结果」口径（我方定案 ＋ 如实声明的边界）**：`b_feed_record`（`spec/schema.sql:329-342`）**只有 `batch_id` / `bag_id` / `feed_weight` / `fed_at` / `operator` / `remark`**，**没有任何检测单外键** ⇒ 数据库**无法复原「投料那一刻的现行单」**。⇒ 口径 = **返回该车次的「现行检测单」**（★ **复用 M5** `internal/store/inspection.go#currentInspectionIDTx`，必要时提取为可导出 helper；`b_obj_void` 已作废单**不算**），并**显式标注** `is_current=true`；★ 若该车次**无合格现行单而以生效紧急放行投料** ⇒ 标 `urgent_release=true` ＋ 两笔留痕。★ **不得**：① 改 schema 加列；② 在返回体/前端**假装**它是历史快照；③ 把「现行单」说成「当时单」。★ 这是**已知的信息缺口**，按 P1「数据不可变」精神，宁可如实标注也不造一个假的历史。
10. ★★ **紧急放行「是否生效」的判定（沿用批 6 口径，不得另写）**：只认 **`s_audit_log`** —— 同一 (`entity='b_truck_lot'`, `entity_id`) 上**同时存在** `action='urgent_release_init'` 与 `action='urgent_release_approve'` 两笔，**且两笔 `actor_open_id` 不同** ⇒ 生效；**只有 `init` 一笔 ⇒ 不生效**。★ 本批**不得**重复实现该判定（★ 建议：**提取 M6 已写好的 `urgentEffectiveTx` 为可导出 helper 复用**，或直接复用其现有形态）。
11. ★★ **让步接收的标注判据（我方定案）**：该批投料链上任一袋**所属车次的现行检测单 `conclusion='CONCESSION'`** ⇒ `concession_used=true`。★ 依据：批 6 `truckReleasedTx` 用的就是 `conclusion ∈ {合格, CONCESSION}` ⇒ **同一套词表，两处必须一致**；★ `b_inspection_result` 的数值照常列出；★ **内部显著标注**（`D20`），**对外不披露**属 M9。
12. ★ **未命中 ≠ 报错**（`TC-M8-03` 明文）：正向 / 反向追溯**查无流向**时返回 **HTTP 200** ＋ **空数组** ＋ `has_flow:false`，★ **不得**返回 404 / 500，★ **不得**把"无流向"显示成 0 条静默 —— 前端要**明确显示"无流向"**。★ 同族判据（`TC-M10-03`「显示『数据未接入』，不是 0」）：**"错误表现为正确"是本项目明文禁止的反模式**。
13. ★ **追溯 / 档案全部只读**：M8 三个模块**零写业务表**（不写 `s_audit_log` 也不算 —— 一期追溯**不记访问日志**，访问日志属 M9 的报告分享）。★ 唯一例外：**无**。
14. ★★ **判据纪律（附加铁律 9）**：凡「在某范围内找某标记」的判据，**必须匹配结构化位置，不得匹配裸词** —— 裸词会被文档里的「讨论」命中而**恒真**。★ 尤其 `A17`（M8 零写）这类**机检**：要**限定到本批新增文件 / 函数**、或按 SQL 关键字 ＋ 表名**结构化匹配**，**不得**用「有没有 `UPDATE` 这个词」。★ 判据自身必须先被探针打过（真阳性保留 / 假阳性消除 / 旧逻辑复现）。
15. ★★ **heredoc 纪律（附加铁律 10）**：`<<EOF` **不加引号**时，体内**反引号与未转义的 `$` 会在本机被执行/展开**。⇒ 要发给远端的脚本，**注释里一律用「」，不要用反引号**；需要远端展开的 `$` 写 `\$`。
16. ★ **不加 `cgo`、不加需要联网下载的重依赖**；如需新增依赖，**先在回执里说明理由**（批 3 引 `skip2/go-qrcode` 走的就是这个流程）。码解析**沿用批 3 的 `internal/codec`**，不得另写一套。
17. ★★ **写 `COLLAB.md` 只能用「局部追加」** —— **禁止整体重写，禁止用格式化工具把该文件整体写回**（已出事故：底本是旧版本 ⇒ 冲掉对方内容）。门禁 `scripts/check_collab_anchors.py` 会判红这类覆盖。
18. ★ **`s_audit_log` 的写法**：`AppendAudit`（`internal/store/audit.go:15`）已由批 1 交付；`entity` / `entity_id` / `action` / `reason` / `actor_*` 齐备（`spec/schema.sql:603-623`）。★ `action` 为 `VARCHAR(32)` ⇒ 本批新增的 `ship_void_init` / `ship_void_approve` **长度合规**（≤ 32）。
19. ★ **测试夹具纪律**（批 3 起沿用，批 6 强调）：跨包共用测试库时，清理必须**按账号精确匹配**（禁用前缀通配）、**先删子表再删父表**；断言「某表为空」必须**限定本用例作用域**，不能全库 `COUNT(*)`。★ 本批父表 `b_shipment` ／ 子表 `b_shipment_item` ⇒ **先删 item 再删 shipment**。
20. ★ **本批不得顺手实现 M9 / M10 的功能**（报告分享 / 对外报告页 / 报表）—— ★ 尤其 **`D20`「对外不披露让步」是 M9 的落点**，本批只做**内部档案标注**。
21. ★ **不允许物理 `DELETE` 任何业务行**（撤销是**状态推进**，明细**保留**）——与批 6「只允许删投料记录」不同，**本批一条都不删**。

---

## 7. 完成后（三条全满足）

1. `COLLAB.md` 中**本批议题段**写回执 ＋ 状态改 `MIMO-DONE`（★ 必须是**行首恰为 `- **状态**：MIMO-DONE`** 的那种行 —— 驱动的判据① 只认这个结构化位置）；
2. **提交代码**（显式路径，**禁 `git add -A`**）；★ **按铁律 12 分阶段提交**（后端一块 / 服务器 TC 全绿 / 变异自证 / 前端，各提交一次）；
3. 提交前 `bash scripts/check_all.sh` **全绿**，并在服务器跑 `bash scripts/run_tc_server.sh`（★ 输出会先打印「包集（N 个）」—— 请把**实际包数**与各包结果写进回执）。

★ 若发现**本批过大**，**回执说明并建议拆分**（`COLLAB.md §2` 允许），**不要硬做完、也不要闷头做一半**。
★ 若发现**规格本身有问题**（`spec/` 或 `docs/04` 的 UC/TC 有矛盾），**开议题**，**不要自己改规格**。

---

## 8. 本批我方（WorkBuddy）已先行指出的易错点汇总

| # | 易错点 | 落在哪条 |
|---|---|---|
| 1 | ★★ 以为 `uk_ship_bag(shipment_id, fg_bag_id)` 能挡住**跨单**重复（它只管**单内**） | §6-2 / A3 |
| 2 | 撤销**发起就改状态**（应 `init` 不改、`approve` 才改） | §6-4 / A6 |
| 3 | 撤销审批**不校验发起人 ≠ 审批人** | §6-4 / A7 |
| 4 | 给 `b_shipment` **自加** void 列 / 自建 void 表（冻结件无此列） | §6-4 |
| 5 | 把**两个客户**的袋塞进同一张出货单（`customer_id` 单值 ⇒ 必拆单） | §6-5 / A4 |
| 6 | 自造 `待出场` 之类**新枚举**（冻结枚举只有 `已出厂` / `已撤销`） | §6-6 |
| 7 | 去写 `b_fg_lot.status='已出货'`（**语义未定义**，会造成两份真相） | §6-7 / §5 |
| 8 | 读入口挂到 **`ship.void.approve`**（该点 **无 `READ`**）⇒ 谁都读不到 | §6-8 / A14 |
| 9 | 反向追溯**假装**返回「历史当时单」（`b_feed_record` 无检测外键，无法复原） | §6-9 / A11 |
| 10 | 把「无流向」做成 **404 / 500** 或静默 0 条（应 200 ＋ `has_flow:false`） | §6-12 / A10 |
| 11 | 正向追溯**绕开 `b_feed_record`**（如直连 `b_fg_lot.batch_id`）⇒ 正反必不互逆 | §1-2 / D4 / A9 |
| 12 | **重写**一套紧急放行生效判定（应复用 M6 / M5 现有实现） | §6-10 |
| 13 | 让步判据**自造词**（应与批 6 一致用 `conclusion='CONCESSION'`） | §6-11 / A12 |
| 14 | M8 追溯里**顺手写库**（含写访问日志 —— 那属 M9） | §6-13 / A17 |
| 15 | 用**裸词**机检「M8 零写」（判据恒真/恒假，铁律 9） | §6-14 / A17 |
| 16 | 撤销时**物理删除** `b_shipment_item`（应保留，靠单状态表达失效） | §6-21 / A3 |
| 17 | 取号写成「先查 max 再 +1」（应 `GET_LOCK` ＋ 同事务） | §6-1 / A15 |
| 18 | 顺手实现 M9 / M10（含 `D20` 对外披露口径） | §5 / §6-20 |
