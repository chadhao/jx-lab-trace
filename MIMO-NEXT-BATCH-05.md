# MIMO-NEXT-BATCH-05 · 批 5：M5 检测

> **交付方**：mimo code ｜ **验收方**：WorkBuddy｜ **派出日**：2026-10-09
> **批次**：批 5 / 8 ｜ **模块**：**M5 检测** ｜ **依赖**：批 1（M0 地基）+ 批 2（M1 主数据）+ 批 3（M3 收货与打码）+ 批 4（M4 取样与留样）—— **四者均已独立验收通过 `AGREED`**（`N-001` / `N-007` / `N-010` / `N-011`）
> ★ **开工前必须完整读 `MIMO-ONBOARDING.md`**（含铁律与禁止事项），再读本文件。

---

## 1. 本批目标（一句话）

**把「测了什么、当时是多少、谁批的」钉死，并让「让步接收」可举证**：检测任务列表 · 批次检测项清单（三态）· 结果录入 + 附件 · 结论与处置 · **让步接收（四字段 + 双签）** · 紧急放行（**发起 ≠ 审批**）· 复检（**不覆盖原结果**）。

★ 本批**只做检测**：**不做生产/投料谱系**（批 6 · M6）· **不做出货追溯**（批 7）· **不做对外报告页**（批 8 · M9）。
★ 本批**最重要的四条**（做错就要返工）：
1. ★★ **检测挂「大样」（`b_inspection.group_id`），不挂袋** —— 一车 30 袋 ⇒ 30 个份样 ⇒ 1 个大样 ⇒ **只对这一个检测**；
2. ★★ **「批次检测项清单」就是 `b_inspection_result` 的行集合**（含 `state='未测'` 的行）—— 不另建表、不另存字段；**三态不可合并**（「未测」是待办，「不适用」是结论）；
3. ★★ **数据不可变（P1）**：结果**只能从「未测」单向写一次**；要改 ⇒ **不原地改**，走「**作废原检测单 + 新开一张检测单**」（见 §6-7）；允许的原地写只有「未测 → 已测 / 不适用」这一次流转；
4. ★★ **让步接收四字段必填 + 双签**（质检方 / 使用部门各签一次，**两个权限点 = 两个入口**）· **紧急放行发起 ≠ 审批** · **复检新开单、原单结果一字不动**。

---

## 2. 必读

| # | 文件 | 读什么 |
|---|---|---|
| 1 | `COLLAB.md` | §0 铁律（★ 含**附加铁律 6~11**）· §1 当前状态 · §2 责任域 · §4 **`N-008` / `N-012`** |
| 2 | `docs/04-模块设计与用例.md` | ★★ **`M5` 的 UC-M5-01~07 / TC-M5-01~14**（这是你的验收标准） |
| 3 | `docs/03-模块划分与实施批次.md` | **批 5 的 6 条验收要点**（§2 表格「批 5」行） |
| 4 | `docs/01-设计定案.md` | **§5.4 取样、检测、留样**（表设计 +「检测挂大样」）· **§6.4 检测**（流程 + 放行单位＝车次 + 让步双签 + 授权的强制留证）· **D6 附件口径** · **D13**（整车/整批结论＝质量加权平均或 AQL，**不是一票否决**）· **D14**（`CONCESSION` 术语）· **D20 / D22**（让步**对外不披露** · 客户沟通记录必填）· **P1**（数据不可变） |
| 5 | `spec/schema.sql` | 本批要动的 **3 张表**：`b_inspection`(474) `b_inspection_result`(504) `b_inspection_file`(524)；另**只读**引用 `b_sample_group`(395) `b_sample`(411) `b_obj_void`(277) `s_audit_log`(603) `m_test_item`(106) `m_test_item_limit`(130) `b_truck_lot`(220) `b_production_batch`(296) `b_fg_lot`(344) —— ★ **列注释里的枚举原词就是取值** |
| 6 | `spec/permission-points.json` | 51 个权限点（★ 本批消费其中的 **11 个 `insp.*`**，**不得新增**）—— ★ 注意紧急放行是**两个点**（`init` 级别 `INIT` / `approve` 级别 `APPROVE`） |
| 7 | `internal/store/limits.go` | ★★ **判定限解析已由批 2 交付**（`ResolveLimit` / `LimitRank`：客户 × 物料，最具体者优先，`0` = 通用默认）—— **必须复用**，不得另写一套 |
| 8 | `internal/store/sampling.go` · `internal/store/retention.go` · `internal/httpapi/sample.go` | ★ 批 4 的**工程范式**：加锁取号 / 单向一次性写 / 两个权限点两个入口 / 审计双记 / 显式路径提交 |
| 9 | `internal/audit/audit.go` · `internal/store/audit.go` | ★ **审计留痕**接口（`audit.Entry{Entity,EntityID,Action,Field,OldValue,NewValue,Actor*,Reason}` / `AppendAudit` / `FindAuditByEntity`）—— 本批**紧急放行**靠它落痕 |
| 10 | `docs/05-环境与调试约定.md` | ★★ **调试只在测试服务器**（见 §6 硬约束一） |

★ 11 个 `insp.*` 权限点**已在 `spec` 登记、常量已在 `internal/permission/code.go` 声明**（`InspTaskView` … `InspUrgentReleaseApprove`），你只需**消费**（路由层引用），**不要**改声明文件或新增点。

---

## 3. 交付物

### D1 · 检测任务列表（`insp.task.view`）

- `GET /api/insp/tasks`：★ 三个来源**并集** —— `b_truck_lot`（车次）· `b_production_batch`（生产批）· `b_fg_lot`（成品批）。
- 每行至少返回：`target_type`（`车次` / `生产批` / `成品批`）· `target_id` · 对象码 `target_code` · 对象自身业务状态（车次取 `b_truck_lot.status`）· **检测态** `insp_state` ∈ `未取样` / `待检` / `已检` · 现行检测单摘要（`inspection_no` / `conclusion` / `disposition`，无则空）。
- ★★ **`insp_state` 判定**：对象无「现行大样」（`b_sample_group` 无行）⇒ `未取样`；有现行检测单但 `conclusion` 为空 ⇒ `待检`；有现行单且有 `conclusion` ⇒ `已检`。
- ★★ **不做「消失」处理**：`未取样` 的对象**照常列出**，并给可读提示（如 `hint: "尚未取样"`）—— 这就是 `TC-M5-14`。
- ★ 过滤：`?state=未取样|待检|已检`（缺省＝全部）。
- ★ **`status='已作废'` 的对象不列出**（车次 `已作废` / 生产批 `已作废` / 成品批 `已作废`）—— 作废对象不再参与业务。
- 权限：**读**入口用 `access.LevelRead`。

### D2 · 检测单：建单 + 挂大样 + 批次检测项清单（`insp.scope.edit`）

- `POST /api/insp`（建单）：body `{target_type, target_id}` ⇒ INSERT `b_inspection` 一行（`inspection_no` 取号见 §6-4；`test_date` 缺省＝服务端当日；`created_by` = 当前操作者）。
- ★★ **必须挂大样**：`group_id` **必填**，取该 `(target_type, target_id)` 的**现行** `b_sample_group.id`；★ 并**反向校验**该取样组的 `target_type`/`target_id` 与被检对象**一致**（防串挂）。
- ★★ **无大样 ⇒ 拒绝建单**（400，提示「尚未取样，无法检测」）—— 但**任务列表照常列出该对象**（D1）。
- `POST /api/insp/:id/items`（加项）：body `{item_ids: [...]}` ⇒ 逐项 INSERT `b_inspection_result`（`state='未测'`，`inspection_id` = 本单，`item_id` 必须存在于 `m_test_item` 且 `status='启用'`、`is_current=1`）。
- `DELETE /api/insp/:id/items/:itemId`（删项）：★★ **仅当被删项自身 `state='未测'`** 才允许；`state ∈ {已测, 不适用}` ⇒ **拒绝**（`TC-M5-02`）。★ 口径见 §6-6（含「为什么是宽读」的定案理由）。
- ★ **清单查询** `GET /api/insp/:id/items`：返回该单**全部** `b_inspection_result` 行（**含 `未测` 行**）+ 每行的字典信息（项目名 / 单位 / 方法 / `value_type`）。
- 权限：`insp.scope.edit`，级别 **`ALL`**（⇒ 守卫用 `access.LevelAll`）。

### D3 · 结果三态录入 + 判定限快照（`insp.result.entry`）

- `PATCH /api/insp/:id/items/:itemId`：body `{state, value_num, value_text, unit, remark}`。
- ★★ **单向一次性写**（P1 的机械落实）：
  - `state='未测'` ⇒ **允许**写一次，写后 `state` 变为 `已测` 或 `不适用`；
  - `state ∈ {已测, 不适用}` ⇒ **再次录入一律拒绝**（409，「结果已录入，如需更改请走修正」）；
  - 实现上 SQL 必须带 `WHERE id = ? AND state = '未测'`，并**校验受影响行数恰为 1**（≠1 ⇒ 拒绝）。★ **不得**写无 `state` 守卫的裸 `UPDATE`。
- ★★ **判定限快照**：`state='已测'` 且 `value_type='数值'` 时，按 **被检对象** 推 `(customer_id, material_id)`（车次 ⇒ `b_truck_lot`；生产批 ⇒ `customer_id` + `input_material_id`；成品批 ⇒ `customer_id` + `output_material_id`），调 **`store.ResolveLimit`**（`internal/store/limits.go`，**复用不得重写**）取最具体有效行 ⇒ 把 `lower_limit` / `upper_limit` **快照**进本行。
- ★ `judge` 判定：两端都有值且 `value_num` 越界 ⇒ `不合格`；在界内 ⇒ `合格`；无限制或 `value_type≠数值` ⇒ 留空（不臆断）。
- ★ `state='不适用'` ⇒ `value_*` / `judge` 一律留空；`state='未测'` ⇒ 待办，计入待办数（见 D1/A6）。
- 权限：`insp.result.entry`，级别 **`ALL`**。

### D4 · 检测附件（`insp.file.upload`）

- `POST /api/insp/:id/files`（`multipart/form-data`）：落盘到 **`cfg.AttachDir`**（`JX_ATTACH_DIR`，缺省 `/srv/jx-lab-trace/attachments`），**库中只存路径**（`b_inspection_file.file_path`），**附件不进数据库**。
- ★★ **单文件上限 20MB**，**可配置**（建议 `JX_ATTACH_MAX_MB`，缺省 20，正整数；非法即拒绝启动 —— 与 `monthsFromEnv` 同一手法）。20MB ⇒ 通过；21MB ⇒ **拒绝**（400/413）（`TC-M5-05` / `TC-M5-06`）。
- ★ 超限须**在读完整文件前**拒绝（先看 `Content-Length` / `MaxBytesReader`），不得先落盘再删。
- 权限：`insp.file.upload`，级别 **`ALL`**。

### D5 · 出结论 + 处置（`insp.conclusion` / `insp.disposition`）

- `POST /api/insp/:id/conclusion`：body `{conclusion, defect_desc?, remark?}`，`conclusion ∈ 合格 / 不合格 / CONCESSION`。
  - ★ `CONCESSION` 时**同一次必须提交四字段**：`authorized_by`（授权人）· `cust_notified_at`（何时告知客户）· `cust_contact`（告知谁）· `cust_channel`（渠道 ∈ `电话/微信/邮件/书面`）。**缺任一 ⇒ 拒绝**（400）（`TC-M5-08`）。
  - ★ **单向一次**：`WHERE id = ? AND conclusion IS NULL`；已有结论再调 ⇒ **拒绝**（409，提示走修正/复检）。
- `POST /api/insp/:id/disposition`：body `{disposition}`，`disposition ∈ 退货 / 换货 / 让步接收 / 返工`。
  - ★ 前置：该单**已有 `conclusion`**（否则 409）。
  - ★ 配套规则：`conclusion='合格'` ⇒ **不得填处置**；`conclusion='不合格'` ⇒ **必填**；`conclusion='CONCESSION'` ⇒ **必须恰为 `让步接收`**。
- `POST /api/insp/:id/concession/qc-sign`：写 `qc_signed_by`（权限 `insp.concession.qc_sign`，级别 **`ALL`**）。★ 前置：本单 `conclusion='CONCESSION'`。
- `POST /api/insp/:id/concession/dept-sign`：写 `dept_signed_by`（权限 `insp.concession.dept_sign`，级别 **`ALL`**）。同前置。
- ★★ 双签**必须是两个入口**（两个权限点分别落给 `qc` 与 `production`，`ALL` 级别 ⇒ 用 `access.LevelAll`，**不是** `INIT`/`APPROVE`）；两次都**单向一次**（已签再签 ⇒ 拒绝）。
- ★★ **同步车次状态**（`target_type='车次'` 时）：`合格` ⇒ `b_truck_lot.status='合格'`；`不合格` ⇒ `'不合格'`；`CONCESSION` ⇒ `'让步接收'`；★ 处置 `退货` ⇒ `'已退货'`。★ 生产批 / 成品批**不回写状态**（其 `status` 枚举中无检测结论）。★ 袋码作废**仍走 M3 的退车登记**（`POST /api/recv/trucks/:id/return`），本批**不重复实现**。
- ★ 内部标注：`CONCESSION` 本身就是内部显著标注（`conclusion='CONCESSION'`）；★ **对外披露口径属批 8**（见 §5）。

### D6 · 紧急放行（`insp.urgent.release.init` / `insp.urgent.release.approve`）

- `POST /api/insp/urgent-release/init`：body `{entity, entity_id, reason}` ⇒ **发起**。`reason`（放行理由）**必填非空**。
- `POST /api/insp/urgent-release/approve`：body `{entity, entity_id, reason?}` ⇒ **审批**。
- `GET /api/insp/urgent-release?entity=&entity_id=`：查放行态（`init_by` / `init_at` / `approved_by` / `approved_at` / `reason`）。
- ★★ **落痕方式（我方定案，见 §6-10）**：落 **`s_audit_log`** —— `entity` = 对象表名（`b_truck_lot` / `b_production_batch` / `b_fg_lot`），`entity_id` = 对象 id，`action` = `urgent_release_init` / `urgent_release_approve`，`actor_*` = 操作者，`reason` = 理由。**不新增表、不新增列**。
- ★★ **发起 ≠ 审批，双重保障**：① 权限层 —— 发起入口守卫 `access.LevelInit`、审批入口守卫 `access.LevelApprove`（`qc` 对 `approve` 为 `NONE` ⇒ 自批 **403**）；② **服务层** —— 审批时若该对象上 `urgent_release_init` 行的 `actor_open_id` **等于**本次审批人 ⇒ **拒绝**（403）。两条都必须有，不得只靠权限层。
- ★ 前置：审批时该对象**必须先有 `init` 行**（无 ⇒ 409「未发起」）；`GET` 走 `insp.task.view` 或同级读权限（`access.LevelRead`）。
- 权限：发起 `insp.urgent.release.init`（`INIT`）· 审批 `insp.urgent.release.approve`（`APPROVE`）。

### D7 · 复检（`insp.scope.edit`）+ 结果修正（`insp.result.correct`）

- **复检** `POST /api/insp/:id/recheck`：⇒ **新开一张检测单**，`is_recheck=1`、`recheck_of` = 原单 id；其余（挂大样、清单、录值、结论）同新单流程。★ **原单一切不动**（`TC-M5-11`：逐字比对原单全部 result 行）。
- **结果修正** `POST /api/insp/:id/correct`：⇒ ① 在 `b_obj_void` 记一行作废原单（`entity='b_inspection'`、`entity_id` = 原单 id、`reason` **必填非空**；★ `approved_by` 留空 —— 更正类**不审批**，只「填原因 + 留痕」，见 `spec/permission-points.json#constraints.approval_scope`）；② **新开一张检测单**（`recheck_of` = 原单 id、`is_recheck=0`），结果**重录**（可复制原值后修改）。★ **原单的 `b_inspection_result` 行一字不动**（这就是 `TC-M5-12` 的红线）。
- ★★ **「现行检测单」判定 = 该 `(target_type, target_id)` 上未被 `b_obj_void` 作废的最新一张**（同 target 可有：原单 / 复检单 / 修正单多张并存）。列表、退车前置、车次状态回写都只认现行单。
- ★★ **联动修正（本批必做，见 §6-9）**：`internal/store/receiving_bag.go` 约 556~560 行的**退车前置查询**（`SELECT id FROM b_inspection WHERE target_type=? AND target_id=? AND disposition='退货'`）**必须排除已作废单** —— 否则「修正后处置已改合格」的车次仍会被判可退车。改动**最小**（加一个 `NOT EXISTS` 子查询），且 **M3 既有全部 TC 必须保持绿**。
- 权限：复检 = `insp.scope.edit`（`ALL`）· 修正 = `insp.result.correct`（`ALL`）。

### D8 · 前端（Vue3）

- **检测任务列表页**：三来源并集 + 检测态过滤 + 「尚未取样」提示。
- **检测单页**：清单（三态可区分）· 逐项录值 · 附件上传（显示上限）· 出结论 / 处置 · 让步四字段表单 + **双签两个动作**（按权限显隐）· 复检 / 修正入口。
- **紧急放行**：发起 / 审批两个动作（按权限显隐）。
- ★ 动作按 `perm-summary` 显隐（沿用批 4 形态：`GET /api/insp/perm-summary` 之类），**服务端仍是唯一权威**（前端隐藏≠服务端放行）。
- ★ 构建产物仍 `//go:embed` 进二进制（`scripts/build.sh` 形态不变）。

### D9 · 门禁保持

- ★ `bash scripts/check_all.sh` **必绿 11 项不得回退**（+ 2 会报项不得出现新命中）。
- ★ `check_perm_registry.py` **必须仍绿** —— 本批新增 **11 个 `insp.*` 权限点的消费端**（每点至少被一处 `RequirePerm(permission.InspXxx, …)` 消费）。
- ★ 本批**不得新增 / 删除任何权限点**（51 × 6 = 306 固定）；**不得改** `spec/*.json` / `spec/schema.sql` / `docs/*`。

---

## 4. 验收判据（WorkBuddy 会逐条独立复核，★ 一律不采信回执）

| # | 判据 | 怎么验 |
|---|---|---|
| A1 | `bash scripts/check_all.sh` **必绿全绿** | 复跑（11/11） |
| A2 | ★★ **检测挂大样、不挂袋** | 建单必带 `group_id` 且与 target 的取样组一致；**无大样 ⇒ 400**；★ 且**任务列表仍列出该对象**并标「尚未取样」（`TC-M5-14`） |
| A3 | 5 项清单 ⇒ **5 行 `state='未测'`** | `TC-M5-01`：按本单作用域计数（★ 不得全库 `COUNT(*)`） |
| A4 | ★★ **删已录值项 ⇒ 拒绝**；★ **删未测项 ⇒ 允许且审计留痕** | `TC-M5-02`（拒）+ 我方口径 §6-6 的另一半（允许） |
| A5 | 已录值时**追加**新项 ⇒ 允许 | `TC-M5-03` |
| A6 | ★ **三态各自可区分**，`未测` 计入待办 | `TC-M5-04`：三项分别 `已测` / `不适用` / `未测`，断言三者互不混淆；待办数 = `未测` 行数 |
| A7 | 附件 20MB 通过 / 21MB 拒绝；**库中只有路径** | `TC-M5-05` / `TC-M5-06`：断言 `file_path` 非空、库里**无文件内容列**、文件确实落在 `AttachDir` |
| A8 | 结论＝让步 ⇒ 持久化为 **`CONCESSION`** | `TC-M5-07`：读库断言字符串恰为 `CONCESSION`（**不是**「让步接收」） |
| A9 | ★★ **让步不填客户沟通记录 ⇒ 拒绝** | `TC-M5-08`：四字段**逐个留空**各测一次，均须拒绝 |
| A10 | 让步四字段 + 双签 ⇒ 成功；两签列均非空 | `TC-M5-09`：★ **对外部分（报告页不出现让步字样）见 §5 —— 载体属批 8**，本批只验数据层 |
| A11 | ★★ **紧急放行发起人自批 ⇒ 拒绝** | `TC-M5-10`：★ **两条都要证** —— qc 调审批入口 ⇒ **403**（权限层）；构造「有 approve 权限者恰为发起人」场景 ⇒ 服务层亦拒绝 |
| A12 | ★ **复检：新单 + 原单结果不变** | `TC-M5-11`：对原单全部 result 行做**规范化快照**（含 `id` / 值 / `state` / `judge`）前后逐字比对 |
| A13 | ★★ **修正不得原地 UPDATE** | `TC-M5-12`：① 已测行二次录入 ⇒ **拒绝**且库中原值不变；② 修正 ⇒ 原单**被作废**（`b_obj_void` 有行）+ **新单**存在且 `recheck_of` 指回原单；★ 变异取证见下 |
| A14 | 任务列表可列出 + 可按状态过滤 | `TC-M5-13` |
| A15 | 11 个 `insp.*` 点**真在路由层被消费** | `check_perm_registry.py` 绿；★ 并**读路由代码**确认不是只在 `all.go` 挂名；★ 级别逐个核对（`init`→`LevelInit`、`approve`→`LevelApprove`、其余→`LevelAll`/`LevelRead`） |
| A16 | **M5 的 14 条 TC 均有自动化测试** | `TC-M5-01~14` 逐条对得上测试函数（测试名带 TC 编号，形如 `TestTC_M5_01_…`；沿用批 4 的 store / httpapi 双层形态） |
| A17 | ★ 服务器真跑全套 TC 全绿 | `bash scripts/run_tc_server.sh`（★ 包集**自动发现**，输出会先打印「包集（N 个）」）；回执须写明**实际包数**与**各包 PASS/FAIL/SKIP** |
| A18 | ★★ **联动修正**：退车前置排除已作废检测单 | 造「已作废单 disposition=退货 + 现行单 disposition=合格」⇒ 退车**必须被拒**；★ **M3 既有全部 TC 保持绿** |
| A19 | ✓ **数据不可变（P1）**：无对 `b_inspection_result` 的**无守卫 UPDATE** | 读实现：`internal/store` 中对 `b_inspection_result` 的 `UPDATE` **必须带** `AND state = '未测'` 且校验受影响行数；`b_inspection` 的结论类字段必须带 `AND conclusion IS NULL` |
| A20 | ✓ **紧急放行两条审计各一笔** | 读 `s_audit_log`：`action='urgent_release_init'` 记**发起人**、`'urgent_release_approve'` 记**审批人**；两笔 `actor_open_id` 不同 |

★ **单点变异自证（必做，≥2 处）**，例如：
- 去掉「让步四字段必填」校验 ⇒ **A9 必须红**（且只有它对应用例红）；
- 把结果录入改成**无 `state` 守卫**的裸 `UPDATE` ⇒ **A13 / A19 必须红**；
- 把紧急放行审批入口的级别从 `LevelApprove` 改成 `LevelAll` ⇒ **A11 必须红**；
- 把建单的 `group_id` 校验去掉（允许无大样直接建单）⇒ **A2 必须红**。

把变异点、红的证据（哪些用例红、哪些保持绿）、还原后的 `sha256` 写进回执。

---

## 5. 明确不做（本批）

| 项 | 为什么 |
|---|---|
| 建生产批 / 投料扫码 / 谱系 / 作业段 / 成品批生成 | **批 6**（M6）—— 本批只能**读**现成对象，不建批 |
| 出货 / 追溯 | **批 7** |
| **对外报告页**（含「报告页不出现让步字样」） | **批 8**（M9）—— ★ `TC-M5-09` 的后半段**其载体（报告页）本批不存在**，故本批只验**数据层**；对外披露口径在批 8 的 M9 用例中**复验**，**不得**在本批自造一个报告页来"凑验收" |
| 整车/整批结论的**质量加权平均 / AQL 判定**（D13） | ★ 本批只落**单次检测单**的结论与处置；加权/AQL 属**报表算法**（批 8 · M10）。★ 判据：本批**不得**实现任何"自动汇总多单"的结论 |
| 留样**到期自动提醒** | **二期**（`docs/03` §4） |
| 仪器直连自动采集（`source='仪器'` 的写入通道） | **二期** —— 本批 `source` 缺省 `人工`，**不实现**仪器写入 |
| 行级 / 列级权限 | 一期不做 |
| 真实飞书回调 | 用户未给凭据（`REMAINING.md#U1`）；沿用 dev 桩 |

★ 本批需要「生产批 / 成品批」对象才能验 D1/D2/D5 —— 二者属批 6。⇒ **允许用测试夹具直写库造最小对象**（与批 3 验退车、批 4 验中间/成品样同一手法）；★ **不得**在本批实现建批入口（那是 M6 的范围）。

---

## 6. 口径要求（易错点，逐条确认）

1. ★ 表结构以 `spec/schema.sql` 为准 —— **列语义不得改**；要改 ⇒ **开议题**（判例见 `N-006` / `N-009`：**以机读件为准**，文档写错则改文档）。
2. ★★ **【硬约束一 · 环境】系统调试一律在测试服务器（`192.168.10.50`）上进行，不在本机（Windows）进行** —— 详见 **`docs/05-环境与调试约定.md`**。
   - ❌ 本机**不得**：跑服务（`go run` / 起监听）、连库做迁移或联调、起临时 MySQL、建隧道、重启服务验判据；
   - ✅ 本机**可以**：`go build`（只编译）、`go test`（★ **纯单元测试，不得依赖外部服务**）、`gofmt` / `go vet`、门禁脚本、只读探针；
   - ✅ 运行与联调一律走：`bash scripts/deploy-test-server.sh --restart --smoke`；批量用例走 `bash scripts/run_tc_server.sh`；
   - ★ **判据（一句话）**：**要 listen 或要改远端状态的，去服务器；只读、只编译、只静态检查的，留本机。**
3. ★★ **【硬约束二 · 提交】远端已接**：`origin` = `git@github.com:chadhao/jx-lab-trace.git`（分支 `main`）。★ **你只做本地提交，不 push** —— **推送由 WorkBuddy 在独立验收通过后执行**。提交用**显式路径**（**禁 `git add -A`**）。
4. ★★ **检测单号 `inspection_no` 取号（我方定，不是草稿）**：`JC` + `YYMMDD`（**服务端建单当日**）+ `-` + **3 位当日序号**（从 `001` 起），例 `JC261009-001`。★ 取号与批 3 `newNoticeNo` / 批 4 样品序号**同一手法**：`GET_LOCK` + 同事务按前缀 `LIKE 'JC261009-%'` 取最大值 +1；★ **超 `999` 明确报错**，**不自动加宽**。★ `uk_inspection_no` 保证唯一。
5. ★★ **检测单「现行」判定（本批一切查询的公共口径）**：同一 `(target_type, target_id)` **可以有多张检测单**（原单 / 复检单 / 修正单），**「现行」= 未被 `b_obj_void` 作废的那一张**（`entity='b_inspection'`）。★ 任务列表、退车前臵、车次状态回写、让步双签的前置，**都只认现行单**。
6. ★★ **「清单」与「删项」**：
   - **清单 = `b_inspection_result` 的行集合**（含 `state='未测'` 的行），**不另建表、不另存字段**；
   - ★ **删项定案（宽读，附理由）**：`UC-M5-02` 写「有结果后只能加项、不能删项」，而 `docs/01` §5.4 给的**立论**是「否则会出现『结果还在、项目没了』的孤儿数据」。⇒ **以立论为准**：**仅当被删项自身 `state='未测'` 时可删**；被删项 `state ∈ {已测, 不适用}` ⇒ **拒绝**（`TC-M5-02`）。★ **不得**放宽成「已测项也能删」——那是本批红线。
   - ★ **删「未测」项 = 物理 `DELETE` 该行**（不是作废）：理由 —— 「未测」行**不含业务事实**（是待办清单项），且 `uk_result_item(inspection_id, item_id)` **不容许**「作废后重加同项」；★ 但**必须写审计**（`action='delete'`，`entity='b_inspection_result'`，`old_value` 记 item 快照），保证「曾经挑过它」这个事实不丢。
7. ★★ **数据不可变（P1）在本批的机械形态 —— 两处「单向一次性写」，一处「作废 + 新增」**：
   - **① 结果录入**：`UPDATE b_inspection_result SET state=?, value_num=?, … WHERE id=? AND state='未测'`，★ **校验受影响行数恰为 1**；已测行再录 ⇒ **拒绝**。这是**唯一**允许的原地写。
   - **② 出结论 / 双签**：`UPDATE b_inspection SET conclusion=?, … WHERE id=? AND conclusion IS NULL`（双签同理带 `AND (qc_signed_by IS NULL OR qc_signed_by='')`），★ 同样校验受影响行数。
   - **③ 修正**：**不原地改**，走「**作废原单（`b_obj_void`）+ 新开单**」（见 §6-8）。
   - ★ 判据：A13 / A19。★ **不得**任何形式的无守卫 `UPDATE`。
8. ★★ **复检与修正的差别（都"新开单"，但语义不同，不得混用）**：

   | | 权限点 | 原单 | 新单字段 | 触发场景 |
   |---|---|---|---|---|
   | **复检** | `insp.scope.edit` | **不动、不作废**（原单仍是合法档案） | `is_recheck=1` + `recheck_of=原单id` | 客户有异议（`UC-M5-07`） |
   | **修正** | `insp.result.correct` | ★ **作废**（`b_obj_void`，`reason` 必填） | `is_recheck=0` + `recheck_of=原单id` | 结果录错要改（`TC-M5-12`） |

   ★ 两者的**新单**都是「本 target 的现行单」（修正后现行单＝新单；复检后现行单＝复检单）。★ **更正类一律不审批**（`approval_scope`：只有留样销毁 / 紧急放行 / 出货单撤销 / 让步双签需要审批）⇒ `b_obj_void.approved_by` **留空**。
9. ★★ **联动修正（必做，改动最小）**：`internal/store/receiving_bag.go` 的退车前置查询必须**排除已作废检测单**（加 `NOT EXISTS (SELECT 1 FROM b_obj_void v WHERE v.entity='b_inspection' AND v.entity_id = b_inspection.id)`）。理由：M5 引入「同 target 多张单」后，若不排除，**修正过的退货判定会被已作废单"复活"**。★ **M3 既有 TC 必须保持全绿**；★ 该改动写进回执（属**集成修正**，不是范围蔓延）。
10. ★★ **紧急放行的落痕方式（我方定案）**：落 **`s_audit_log`**，**不新增表、不新增列**。理由：① `spec/schema.sql` 是**冻结件**（38 表，D26 已实测通过）；② `docs/01` §6.4 对该通道的定位原文是「**与其让人绕过系统，不如给一条留痕的路**」—— 本质就是**留痕**，而审计表正是为此而设（只增不改）；③ 发起 / 审批各一笔，`actor_open_id` 天然区分两人，`idx_audit_entity` 可查。★ 若你认为**必须**独立表 ⇒ **开议题**，**不得自改 schema**。★ 附带影响（告知，本批不做）：M6 的「未出结论不得投料」前置若要判「是否已紧急放行」，需查审计表。
11. ★★ **紧急放行「发起 ≠ 审批」必须双重保障**：权限层（`INIT` / `APPROVE` 守卫，`qc` 无 `APPROVE` ⇒ 自批 403）**＋** 服务层（`init` 行 `actor_open_id` == 本次审批人 ⇒ 拒绝）。★ **仅靠权限层不算通过**（`management` 同时也可能有 `INIT` 的一天，硬编码"只有 qc 能发起"是错的假设）。
12. ★ **权限级别的取用（逐点核对，易错）**：
    - `insp.urgent.release.init` ⇒ **`access.LevelInit`**；`insp.urgent.release.approve` ⇒ **`access.LevelApprove`**（★ `ALL` 蕴含一切级别 ⇒ **绝不能**给这两个入口用 `ALL`，否则 `qc` 的 `NONE` 被绕过）；
    - `insp.concession.qc_sign` / `insp.concession.dept_sign` 的级别是 **`["ALL","NONE"]`** ⇒ 用 **`access.LevelAll`**（**不是** `INIT`/`APPROVE`）；
    - `insp.task.view` 是读入口 ⇒ `access.LevelRead`；其余写入口 ⇒ `access.LevelAll`。
13. ★ **枚举取值一律用 `spec/schema.sql` 列注释里的中文原词**：`conclusion ∈ 合格/不合格/CONCESSION`（★ `CONCESSION` 是**唯一**的英文码，界面显示「让步接收」）· `disposition ∈ 退货/换货/让步接收/返工` · `state ∈ 未测/已测/不适用` · `method ∈ 全检/抽检` · `cust_channel ∈ 电话/微信/邮件/书面` · `target_type ∈ 车次/生产批/成品批`。★ **不得自造英文码**（与 M3 的 `disposition='退货'`、M4 的 `role='份样'` 同一口径）。
14. ★ **三态不可合并**：「未测」= **待办**，「不适用」= **结论**。⇒ `state='不适用'` 时 `value_*` / `judge` **必须留空**；★ **不得**用"空值"同时表示「没测」与「不用测」。
15. ★ **判定限必须复用 `internal/store/limits.go` 的 `ResolveLimit`**（批 2 已交付并通过验收）—— **不得**另写一套优先级逻辑；★ 取值维度是 **目标对象的 `(customer_id, material_id)` × `m_test_item`**（车次 ⇒ `b_truck_lot` 的 customer/material；生产批 ⇒ `customer_id` + `input_material_id`；成品批 ⇒ `customer_id` + `output_material_id`）。★ 快照进 `lower_limit` / `upper_limit`（注释原文：「**取值时点的判定限快照**」）。
16. ★ **附件**：目录取 `cfg.AttachDir`（`JX_ATTACH_DIR`）；★ **上限 20MB 且可配置**（**不得硬编码**）；★ **库中只存路径**；★ 超限**先拒后写**，不留半截文件。
17. ★ **车次状态回写**（§3-D5）：仅 `target_type='车次'`；★ 生产批 / 成品批**不回写**。★ 袋码作废仍走 M3，**不重复实现**。
18. ★★ **本批不得新增权限点** —— 只用 `spec/permission-points.json` 已登记的 **51 条**；本批应消费 **11 个 `insp.*`**：`insp.task.view` / `insp.scope.edit` / `insp.result.entry` / `insp.result.correct` / `insp.file.upload` / `insp.conclusion` / `insp.disposition` / `insp.concession.qc_sign` / `insp.concession.dept_sign` / `insp.urgent.release.init` / `insp.urgent.release.approve`；每个受保护入口**必须引用已登记的权限点常量**（禁裸写字符串）。
19. ★★ **写 `COLLAB.md` 只能用「局部追加」** —— **禁止整体重写，禁止用格式化工具把该文件整体写回**（已出事故：底本是旧版本 ⇒ 冲掉对方内容）。门禁 `scripts/check_collab_anchors.py` 会判红这类覆盖。
20. ★★ **判据纪律（附加铁律 9）**：凡「在某范围内找某标记」的判据，**必须匹配结构化位置，不得匹配裸词** —— 裸词会被文档里的「讨论」命中而**恒真**。判据自身必须先被探针打过（真阳性保留 / 假阳性消除 / 旧逻辑复现）。
21. ★★ **heredoc 纪律（附加铁律 10）**：`<<EOF` **不加引号**时，体内**反引号与未转义的 `$` 会在本机被执行/展开**。⇒ 要发给远端的脚本，**注释里一律用「」，不要用反引号**；需要远端展开的 `$` 写 `\$`。
22. ★ **不加 `cgo`、不加需要联网下载的重依赖**；如需新增依赖，**先在回执里说明理由**（批 3 引 `skip2/go-qrcode` 走的就是这个流程）。★ 附件上传**优先用标准库**（`mime/multipart` + `http.MaxBytesReader`），不要为此引第三方框架。
23. ★ **测试夹具纪律**（批 3 踩过、批 4 沿用）：跨包共用测试库时，清理必须**按账号精确匹配**（禁用前缀通配）、**先删子表再删父表**；断言「某表为空」必须**限定本用例作用域**，不能全库 `COUNT(*)`。
24. ★ **「尚未取样」不是错误**：`insp_state='未取样'` 是**正常态**（`TC-M5-14`），列表要**照常返回**它；★ **不得**把它过滤掉、也**不得**在建单失败时把对象藏起来。
25. ★ **不允许物理 `DELETE` 除「未测清单项」以外的任何行**；★ 修正 / 复检**一律新开单**，**不得**改历史单的任何业务字段。
26. ★ **M5 不得顺手实现 M6/M7 的功能**（建批、投料、谱系、出货）—— 需要这些对象时**用测试夹具直写库造**。

---

## 7. 完成后（三条全满足）

1. `COLLAB.md` 中**本批议题段**写回执 + 状态改 `MIMO-DONE`（★ 必须是**行首恰为 `- **状态**：MIMO-DONE`** 的那种行 —— 驱动的判据① 只认这个结构化位置）；
2. **提交代码**（显式路径，**禁 `git add -A`**）；
3. 提交前 `bash scripts/check_all.sh` **全绿**，并在服务器跑 `bash scripts/run_tc_server.sh`（★ 输出会先打印「包集（N 个）」—— 请把**实际包数**与各包结果写进回执）。

★ 若发现**本批过大**，**回执说明并建议拆分**（`COLLAB.md §2` 允许），**不要硬做完、也不要闷头做一半**。
★ 若发现**规格本身有问题**（`spec/` 或 `docs/04` 的 UC/TC 有矛盾），**开议题**，**不要自己改规格**。

---

## 8. 本批我方（WorkBuddy）已先行指出的易错点汇总

| # | 易错点 | 落在哪条 |
|---|---|---|
| 1 | 把检测挂到**袋**上（应挂**大样**） | §6-5 / A2 |
| 2 | 把「三态」压成两态或空值（「未测」与「不适用」混为一谈） | §6-14 / A6 |
| 3 | 结果录入写成**无守卫的裸 UPDATE**（原地改已测行） | §6-7 / A13 / A19 |
| 4 | 修正走原地改，而不是「作废 + 新开单」 | §6-7⑥ / §6-8 / A13 |
| 5 | 复检误把原单**作废**（复检**不作废**原单） | §6-8 / A12 |
| 6 | 让步四字段**只校验一部分**（四字段必须逐个可测） | A9 |
| 7 | 让步双签做成**一个入口**（两个权限点必须两个入口） | §3-D5 / §6-12 |
| 8 | 紧急放行**只靠权限层**、缺服务层同人校验 | §6-11 / A11 |
| 9 | 紧急放行入口误用 `ALL` 级别（应 `INIT` / `APPROVE`） | §6-12 |
| 10 | 忘了**联动修正** M3 退车前置 ⇒ 已作废单"复活" | §6-9 / A18 |
| 11 | 判定限**另写一套**（应复用 `ResolveLimit`） | §6-15 |
| 12 | 附件上限**硬编码**、或先落盘再判超限 | §6-16 / A7 |
| 13 | 「尚未取样」的对象被列表**过滤掉** | §6-24 / A2 |
| 14 | 为了"凑 `TC-M5-09` 后半段"**自造一个对外报告页** | §5 |
