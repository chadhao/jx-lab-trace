# MIMO-NEXT-BATCH-08 · 批 8（**本期最后一批**）：M9 报告分享 + M10 报表

> **交付方**：mimo code ｜ **验收方**：WorkBuddy｜ **派出日**：2026-10-09
> **批次**：批 8 / 8 ｜ **模块**：**M9 报告分享 ＋ M10 报表** ｜ **依赖**：批 1～批 7 **七者均已独立验收通过 `AGREED`**（`N-001` / `N-007` / `N-010` / `N-011` / `N-012` / `N-013` / `N-014`）
> ★ **开工前必须完整读 `MIMO-ONBOARDING.md`**（含铁律与禁止事项），再读本文件。

---

## 0. 本轮说明（先读这段再读 §1）

- **本轮基线**：`HEAD = origin/main = ed8816d`（2026-10-09 19:2x，批 7 验收已推送）；工作区**干净**；门禁 **11/11 全绿**（WorkBuddy 实测）；服务器全套 TC **170 PASS / 0 FAIL / 0 SKIP**。
- ★★ **本批是 8 批计划的最后一批** ⇒ 做完后 `收货 → 取样 → 检测 → 生产 → 出货 → 追溯 → **对外报告分享 → 报表**` 全链闭合，一期功能面收口。
- ★ 请带上既有三条纪律（`COLLAB.md §0` 附加铁律 11 / 12）：

| # | 纪律 | 为什么 |
|---|---|---|
| **A** | **分阶段本地提交**：每完成一块（后端一块 / 静态服务 / 服务器 TC 全绿 / 变异自证 / 前端）**立刻用显式路径提交一次**，**不要攒到最后一次性提交** | 派工轮被整树回收是**常态风险**（批 5 / 批 7 都撞上）；分批提交后**任何一次回收最多只丢最后一块** |
| **B** | **不要用 `git stash` / `git checkout` 处理未提交的 WIP** | 本仓库 `core.autocrlf=true` 且无 `.gitattributes` ⇒ stash 落成 **CRLF** ⇒ `gofmt -l` 判红、门禁 1/11 失败（2026-10-09 实测踩中） |
| **C** | ★ **回执与状态必须写**：任务包 §7 要求把回执写进 `COLLAB.md §4 N-015` 段并把状态行改成**行首恰为** `- **状态**：MIMO-DONE` | ★ 批 7 的 mimo **在写回执前被整树回收**（驱动 18:14 终止），回执缺位、状态卡在 `OPEN` ⇒ 只能由 WorkBuddy 独立验收补位。**本批请在每完成一块后就把回执草稿落到 `COLLAB.md`**，别留到最后 |

★ **本条不改变**「只本地提交、不 push」（硬约束二）。

---

## 1. 本批目标（一句话）

**把检验结果变成能发出去的东西，并且不让公网回头看内网**：按批次生成一份**自包含的静态快照报告**（唯一链接、可设有效期、可撤销、访问留痕），外加一个**只读**的报表模块（5 张基础报表 + 导出）。

★ 本批**最要命的四条**（做错就要返工）：

1. ★★★ **`D20` 对外不披露让步**：报告页**一律不得出现「让步」字样或 `CONCESSION`** —— 无论该批是否用过让步接收料。★ 让步**内部档案**（M8 `GetBatchArchive`）**照旧显著标注**，两者**同时成立**、互不干扰。
2. ★★★ **公网侧只有静态文件、不连内网库**：服务报告快照的**进程**必须**零数据库连接**（不得 import / 不得读 DSN）⇒ 有效性判定**不能**靠"请求时查库"，只能靠**文件是否在可服务目录里**（撤销 / 过期 ⇒ **把快照移出可服务目录** ⇒ 链接自然 404）。
3. ★★ **刷新 = 新 token 新链接**：刷新**不得**覆盖同一 URL 的内容（否则"同一个链接昨天和今天内容不一样"，证据价值归零）⇒ 刷新 = **新建一条记录（新 token）＋ 旧记录置 `已撤销`**。
4. ★★ **M10 只读 ＋ 空数据 ≠ 0**：报表模块**零写业务表**（可机检）；**某数据源整体无数据**时返回 `has_data:false` ＋ `"数据未接入"`，**不得**返回 0 / 空数组冒充"结果为零"（`TC-M10-03`；同族判例 `TC-M8-03`「无流向不是报错」）。

---

## 2. 必读

| # | 文件 | 读什么 |
|---|---|---|
| 1 | `COLLAB.md` | §0 铁律（★ 含**附加铁律 6~12**）· §1 当前状态 · §2 责任域 · §4 **`N-015`**（本批议题）· §4 `N-014`（批 7，含 M8 读实现可复用点） |
| 2 | `docs/04-模块设计与用例.md` | ★★ **`M9` 的 UC-M9-01~04 / TC-M9-01~08** ＋ **`M10` 的 UC-M10-01~02 / TC-M10-01~04**（这是你的验收标准，共 **6 UC / 12 TC**） |
| 3 | `docs/03-模块划分与实施批次.md` | **批 8 的 5 条验收要点**（§2 表格「批 8」行，第 ④ 条 = 公网侧只有静态文件、不连内网库） |
| 4 | `docs/01-设计定案.md` | **§5.5 出货、报告、审计**（★ `b_share_report` / `b_share_access` 关键字段）· **§6.6 追溯与分享**（生成 / 刷新 / 生命周期 / token 原文）· **§11.2 `D20`**（让步：**对外不披露 / 内部显著标注**）· **`P4`**（报表层与业务层解耦）· **`P1`**（数据不可变）· §7 页面清单第 **9 / 10** 行 · §8 权限矩阵「报告」「报表」两行 |
| 5 | `spec/schema.sql` | ★ 本批**唯一的两张表**：**`b_share_report`(570)** `b_share_access`(591)（★ 列注释里的**枚举原词就是取值**：`status` = `有效` / `已撤销` / `已过期`）；只读引用：`b_production_batch`(296) `b_batch_operation`(312) `b_feed_record`(329) `b_fg_lot`(344) `b_fg_bag`(363) `b_bag`(247) `b_truck_lot`(220) `b_inspection`(474) `b_inspection_result`(504) `b_shipment`(541) `b_shipment_item`(559) `b_sample_retention`(434) `m_customer`(27) `m_material`(83) `s_audit_log`(603) |
| 6 | `spec/code-rules.json` | ★★ **确认：`object_types` 只有 `A`~`E` 五类，`report` / `出货` / `报表` 关键字零命中** ⇒ **`report_no` 不是追踪码**（本批开工前已核） |
| 7 | `spec/permission-points.json` | 51 个权限点（★ 本批消费其中 **4 个**：`report.generate` / `report.share.manage` / `rpt.view` / `rpt.export`，**不得新增**）—— ★★ **注意两处 levels 陷阱**：`report.generate` = `["ALL","NONE"]`、`report.share.manage` = `["ALL","NONE"]` ⇒ **两者都没有 `READ`**；`rpt.view` = `["ALL","READ","NONE"]`（**本批唯一含 `READ` 的点**） |
| 8 | `internal/store/trace.go` | ★★ **M9 报告内容的数据来源**：`GetBatchArchive`（D6 批次档案）**已由批 7 交付并验收** —— ★ **复用它的读实现**，但**必须过白名单投影**（见 D1 / §6-3） |
| 9 | `internal/store/shipment.go` | ★★ **取号范式**：`acquireSeqLock`（`GET_LOCK`）＋ **同一事务**内前缀取 max+1 ＋ 超限 `ErrShipSeqOverflow`（`report_no` 取号**沿用同一手法**） |
| 10 | `internal/store/audit.go` | `AppendAudit` / `appendAuditTx`（审计写入口，批 1 交付） |
| 11 | `scripts/deploy-test-server.sh` | ★★ 本批要**增加第二个进程**（`reportd`）的部署与启停；★ 注意 `[2.5/4]` 兜底扫描**按 `/proc/<pid>/exe` 路径精确匹配**（只停本目录二进制）—— **新进程要一并纳入**，否则旧 `reportd` 停不掉 |
| 12 | `docs/05-环境与调试约定.md` | ★★ **调试只在测试服务器**（见 §6 硬约束一） |
| 13 | `internal/store/m6_test.go` · `m7_test.go` · `m8_test.go` | ★ 测试夹具纪律与双层形态（store / httpapi） |

★ 4 个权限点**已在 `spec` 登记、常量已在 `internal/permission/code.go:64-66` 声明**（`ReportGenerate` / `ReportShareManage` / `RptView` / `RptExport`），你只需**消费**（路由层引用），**不要**改声明文件或新增点。

---

## 3. 交付物

### D1 · 生成报告（`report.generate`，级别 `LevelAll`）

- `POST /api/report/generate`：body `{scope_type, scope, title?, expires_at?}`
  - ★ **`scope_type` 一期只支持 `"按批次"`**（逐字取自 `b_share_report.scope_type` 列注释枚举 `按批次 / 按车次`）；`scope` = `{batch_id:<正整数>}`。★ `"按车次"` **一期不做**（见 §5），传其它值 ⇒ **400**。
  - `expires_at`：可空；**缺省 = 生成时刻 + 30 天**（★ 见 §6-6 的语义定义）。
  - ★★ **快照内容 = 对外白名单投影**（**不是**直接把 M8 的读模型序列化出去）：
    - **白名单（只允许这些）**：报告编号 · 生成时间 · 有效期 · 客户名称 · 输入物料 · 计划产出物料 · 生产批**人读行**与生产日期 · 投料明细（吨袋**人读行** · 投料量 · 投料时间）· 作业段（段序 · 班组 · 时段 · 本段产出）· 成品批（**人读行** · 产出物料 · 净重）· 成品袋（**人读行** · 净重 · 状态）· **检测结果表**（检测项目名 · 数值 · 单位 · 判定）· 出货单（单号 · 状态 · 出场时间 · 车牌 · 客户）
    - ★★★ **黑名单（一律不得出现，兼 `TC-M9-05` / `TC-M9-06` / `TC-M9-08`）**：
      1. **「让步」/「CONCESSION」/「让步接收」字样及其任何变体**（`D20` 用户定案）；
      2. **其它客户的数据**（页内只能含所选批次所属客户 —— 生产批天然单客户，但仍须**用例**验）；
      3. **内部未公开字段**：`remark`（生产批/投料/档案备注）· `operator` / `created_by` / `generated_by` 等**内部操作痕迹** · `b_obj_void` **作废留痕** · **紧急放行**留痕（`urgent_*`）· **审计**信息 · **样品/留样**信息 · 班组**人员**姓名。
    - ★ **实现建议（把白名单交给类型系统，而不是靠人查）**：定义**专用的快照投影结构体**（只含白名单字段），渲染只从它取值；**禁止**直接复用 M8 的 `BatchArchive` / `TraceFeed` 等**含内部字段**的读模型做序列化。
  - **`report_no` 取号（我方定案）**：**非追踪码**（`spec/code-rules.json` 只有 A~E 五类对象）⇒ 形态 = **`RP` ＋ `YYMMDD` ＋ `-` ＋ 当日 3 位序号**（例 `RP261009-001`）；取号 = **命名锁 `GET_LOCK` ＋ 同一事务**内按前缀取最大 +1，**超 999 ⇒ `ErrReportSeqOverflow`**（**不自动进位**）。★ 与 M7 `CH…`（`internal/store/shipment.go#nextShipNo`）**同一手法**。
  - **`token`（我方定案）**：**≥32 字符**的**长随机串**（`crypto/rand`，URL-safe 字母表，如 base64url 去填充 32~43 字符）；`uk_report_token` 唯一 ⇒ 碰撞**重试 ≤5 次**，仍失败 ⇒ 500。★ **不得**用时间戳 / 序号 / report_no 派生（可猜测 = 链接可枚举）。
  - **落盘（我方定案）**：
    - 目录 `JX_REPORT_DIR`（默认 `~/jx-lab-trace/reports`）；布局：**`served/`**（可服务目录，静态服务器**只**服务它）/ **`_inactive/`**（撤销 / 过期的快照）/ `access.log` / `access.log.synced`。
    - 文件名 = **`<token>.html`**（★ **不用 `report_no` 做文件名** —— 否则链接可枚举）；★ 写入 = **先写 `*.tmp` 再 `rename`**（原子，防半截页面被读到）。
    - `snapshot_path` 落**可服务目录内的相对路径**（如 `served/<token>.html`）；`url` = `JX_REPORT_PUBLIC_BASE` ＋ `/<token>.html`（默认 `http://127.0.0.1:18090/r`）。
  - `status` = **`有效`**（★ 逐字取自列注释枚举 `有效 / 已撤销 / 已过期`）；`generated_by` / `created_by` = actor。
  - 审计：`entity='b_share_report'` · `entity_id` · `action='report_generate'`。
- 读入口：`GET /api/report/list`（列表，可按 `?status=` 过滤）· `GET /api/report/:id`（详情，含快照字段摘要）—— ★★ 级别 **`LevelRead`**，且**只能挂 `rpt.view`**（见 D3 / §6-7）。

### D2 · 刷新报告（`report.generate`，级别 `LevelAll`）

- `POST /api/report/:id/refresh`：**重新生成快照**。
- ★★★ **口径（我方定案）：刷新 = 新 token 新链接** ——
  1. **新建**一条 `b_share_report`（新 `report_no`、**新 token**、新快照文件、新 `generated_at`，`expires_at` 沿用原值；原行 `title` 与 `scope_*` 沿用）；
  2. **旧行 `status` 置 `已撤销`**，并把**旧快照移入 `_inactive/`**；
  3. 审计**两笔**：旧行 `action='report_refresh_supersede'`、新行 `action='report_generate'`。
- ★ **不得**改写同一行 / 复用同一 token（那正是"同一 URL 内容悄悄变了"）。
- ★ 返回体须同时给出**新**链接，并标注 `superseded_report_no`。

### D3 · 生命周期：设有效期 / 撤销 / 过期清扫（`report.share.manage`，级别 `LevelAll`）

- `POST /api/report/:id/expires`：body `{expires_at}`（★ 语义见 §6-6）—— 改有效期；审计 `action='report_set_expires'`。
- `POST /api/report/:id/revoke`：body `{reason}` —— **`reason` 必填非空**；置 `status='已撤销'` ＋ **快照移入 `_inactive/`**；审计 `action='report_revoke'`（`reason` 落 `reason` 列）。★ **无权限点要求"发起 / 审批"两段式** ⇒ 本批撤销**单步生效**（与 M7 出货单撤销**不同**，不要照抄）。
- `POST /api/report/expire/sweep`：★ **命令式、幂等** —— 扫 `status='有效'` 且已过期的行 ⇒ 逐条置 `已过期` ＋ 快照移入 `_inactive/` ＋ 审计 `action='report_expire'`；返回 `{swept:<n>, report_nos:[…]}`。★ **可重复跑**（第二次返回 `swept:0`）。
- 读入口：`GET /api/report/:id/access`（访问日志，倒序）—— 级别 **`LevelRead`**（挂 `rpt.view`，见 §6-7）。
- ★★ **「不可访问」的落实**：以上三个动作（set-expires 不适用 / revoke / expire-sweep）中，凡**离开「有效」态**者，**必须把快照移出 `served/`** ⇒ 公网侧（静态服务器）**天然 404**，**不需要**它连库判断。★ 只改 `status` 不移文件 ⇒ `TC-M9-03` 必红。

### D4 · 访问日志（`report.share.manage`）

- ★★ **公网侧 = 独立静态服务进程 `reportd`（新增 `cmd/reportd`）**：
  - 用 `net/http` + `http.FileServer` 提供 `JX_REPORT_DIR/served/` 下的静态文件；
  - 路由：`GET /r/<token>.html` ⇒ 命中则 200（`text/html; charset=utf-8`），未命中 ⇒ **404**；`GET /healthz` ⇒ `ok`（供部署 smoke）；
  - ★★★ **零数据库连接**：**不得** import 任何 SQL 驱动、**不得**读 `JX_DB_DSN`、**不得** import `internal/store`（★ 机检：`go list -deps ./cmd/reportd` 中不得出现 `database/sql` / `github.com/go-sql-driver/mysql`）；
  - 每次**访问报告**（不含 `/healthz`）向 `JX_REPORT_DIR/access.log` **追加一行 TSV**：`<RFC3339Nano>\t<token>\t<remote_ip>\t<http_status>\t<user_agent>`。★ **UA / token 里的制表符与换行必须转义**（防日志注入 —— 与附加铁律 10/13 同族）。
  - 监听地址由 `-addr` 决定，默认 **`127.0.0.1:18090`**（★ **不得**绑 `0.0.0.0`，与主服务同口径）。
- ★★ **内网侧同步入库**：`POST /api/report/access/sync`（`report.share.manage`，`LevelAll`）：
  - 读 `access.log` ⇒ 解析每行 ⇒ 由 `token` 反查 `b_share_report.id` ⇒ `INSERT INTO b_share_access`；
  - ★ **幂等**：以 **`(report_id, accessed_at, ip)`** 去重（重复跑不产生重复行）；
  - ★ **不回放**：同步完成的行使日志文件**滚动/落盘偏移**（建议把 `access.log` **`rename` 成 `access.log.synced`** 再重建新 `access.log`，或记录 byte offset 到文件）—— ★ 但 **`rename` 与 `reportd` 的追加写并发时要小心**（`reportd` 按住 fd 继续写旧 inode ⇒ 会丢新行）。⇒ **推荐 byte-offset 方案**（同步状态文件 `access.log.offset`），或由 `reportd` 收到信号后自己滚动。★ 你自行选一种并在回执里说明**为什么**。
  - 返回 `{parsed, inserted, skipped, offset}`。
  - ★ **未命中的 token 行**（本就 404）⇒ 计 `skipped`，**不**中断、**不**写库。

### D5 · M10 报表（`rpt.view` / `rpt.export`）

- `GET /api/rpt/:name`：一期 **5 张**，`name ∈ {quality-trend, customer-recon, output-yield, sample-expiry, nonconform-stat}`；通用筛选 `?from=&to=&customer_id=`（★ 非法值 ⇒ 400）。
- ★★ **只读**：M10 代码**零写业务表**（`A17` 机检口径见 §6-9）。
- ★★ **空数据 ≠ 0**（`TC-M10-03`，本批最高优先判据之一）：
  - 返回体固定含 **`has_data`** 布尔；★ **区分两种态**：
    - **`has_data=false`** ⇒ **该数据源整体无数据**（如全库从未有过检测结果 / 从未有过留样）⇒ 附 **`note:"数据未接入"`**；
    - **`has_data=true` ＋ `rows:[]`** ⇒ 有数据但**本筛选窗口内无匹配** ⇒ 附 **`note:"本条件下无记录"`**。
  - ★ **禁止**：无数据时返回 `rows:[]` 且不给 `has_data` / 或把 `has_data` 恒真 / 或用 `0` 冒充"结果为 0"。★ **前端两句话术必须不同**。
- 5 张表的**口径**（我方定案，落在 `docs/04` 已给的范围内）：
  1. **`quality-trend`** 质量趋势：按**时间桶**（month / week，`?bucket=` 缺省 `month`）统计**检测合格率**（`conclusion='合格'` ÷ 已出结论单数；★ **让步 `CONCESSION` 不计入合格**，与批 6 `truckReleasedTx` 同一词表）；★ 同时给出 `total` / `qualified` / `concession` / `unqualified`;
  2. **`customer-recon`** 客户对账：按客户统计**出货吨位**（**∑ `b_fg_bag.net_weight`**，仅计**已出场**（`ship_at IS NOT NULL`）且**未撤销**的出货单的袋）与**单数**;
  3. **`output-yield`** 产量与合格率：按生产批统计投入物料 / 产出物料 / 投入量 / 产出量 / 产出率（★ `投入量=0` ⇒ 产出率返回 `null`，**不得** 0 或 Infinity）;
  4. **`sample-expiry`** 留样到期：`b_sample_retention` 中按 `expires_at` 升序，标 `expired:true|false`；★ 无留样 ⇒ `has_data=false` ＋ 「数据未接入」;
  5. **`nonconform-stat`** 不合格统计：按检测项目统计**不合格项次**与**占比**（`judge` 为不合格的结果行）。
- `POST /api/rpt/export`：body `{name, filters}` ⇒ 返回 **CSV**（`text/csv; charset=utf-8`，**带 UTF-8 BOM**，便于 Excel 直接打开）；★ **写审计** `entity='rpt.<name>'` · `action='rpt_export'` · `new_value` 记筛选条件；★ **只写 `s_audit_log`**，**不写任何业务表**（`UC-M10-02` 明确"导出写审计"）。

### D6 · 前端（Vue3）

- **「报告」页**：生成（选生产批 → 提交）· 报告列表（单号 / 批次 / 状态 / 有效期 / 链接）· **复制链接** · 刷新（提示"旧链接将失效"）· 撤销（带原因）· 设有效期 · 访问记录查看。
  - ★ 动作按权限显隐（沿用批 4~批 7 形态，`GET /api/report/perm-summary` 或同类），**服务端仍是唯一权威**。
  - ★ **页面不得出现「让步」字样**（前端也不许硬编码该词）。
- **「报表」页**：5 张报表 tab ＋ 筛选 ＋ 导出按钮；★ **空数据显「数据未接入」**，与「本条件下无记录」**文案不同**；★ 不合格项用**红**（本项目为中文环境，合格/不合格建议用中性+红，不要用绿表示不合格）。
- ★ 构建产物仍 `//go:embed` 进主二进制（`scripts/build.sh` 形态不变）。

### D7 · 静态服务与部署

- 新增 **`cmd/reportd`**（见 D4）；★ 保持**零 DB 依赖**。
- `.env` 新增（★ 取值**加双引号**，`N-004` 判例）：`JX_REPORT_DIR` · `JX_REPORT_PUBLIC_BASE` · `JX_REPORTD_ADDR`（默认 `127.0.0.1:18090`）；`.env.deploy.example` 同步。
- `scripts/deploy-test-server.sh`：增加 `reportd` 的**部署 / 启停 / 兜底扫描**（独立 `reportd.pid`；`--restart` 一并重启；★ 兜底扫描**按 `/proc/<pid>/exe` 精确匹配**，**只停本目录的 `reportd`**）；smoke 段**分列**两进程（主服务 `/healthz` 200 且 `version == HEAD`；`reportd` `/healthz` 200）—— ★ **不得**把两者断言混成一个。
- ★ 服务器上**不要**去改 `/etc/caddy`（**该项目无免密 sudo**，改不了也不该改）—— 一期以 `reportd` 承接；★ Caddy / 对象存储属**二期**口径（见 §5）。

### D8 · 门禁保持

- ★ `bash scripts/check_all.sh` **必绿 11 项不得回退**（＋ 2 会报项不得出现新命中）。
- ★ `check_perm_registry.py` **必须仍绿** —— 本批新增 **4 个权限点的消费端**。
- ★ 本批**不得新增 / 删除任何权限点**（51 × 6 = 306 固定）；**不得改** `spec/*.json` / `spec/schema.sql` / `docs/*`。
- ★ **M10 零写业务表**；★ **M9 的 `reportd` 零 DB 连接**。

---

## 4. 验收判据（WorkBuddy 会逐条独立复核，★ 一律不采信回执）

| # | 判据 | 怎么验 |
|---|---|---|
| A1 | `bash scripts/check_all.sh` **必绿全绿** | 复跑（11/11） |
| A2 | ★ **生成报告**：快照落盘 ＋ 唯一链接 ＋ 内容含追溯链与检测表 | `TC-M9-01`：读库断言 `b_share_report` 1 行、`report_no` 形态 `^RP\d{6}-\d{3}$`、`status='有效'`、`token` 长度 ≥32；★ 断言**快照文件存在**于 `served/` 且**非空** |
| A3 | ★★ **刷新 = 新 token 新链接** | `TC-M9-02`：刷新后断言 ① **新行 token ≠ 旧行 token**；② **旧行 `status='已撤销'`**；③ **旧快照已移出 `served/`**；④ 新快照可服务 |
| A4 | ★★ **撤销后不可访问** | `TC-M9-03`：撤销 ⇒ 断言 `status='已撤销'` ＋ **快照已移出 `served/`**（★ 并对 `reportd` 真发一次请求 ⇒ **404**，不是 200） |
| A5 | ★★ **有效期边界** | `TC-M9-04`：`expires_at` = **今日 23:59:59.999** ⇒ sweep 后**仍 `有效`**、快照**仍在 `served/`**；`expires_at` = **昨日 23:59:59.999** ⇒ sweep 后 `已过期` ＋ 快照**已移出**；★ 且 sweep **可重复跑**（第二次 `swept:0`） |
| A6 | ★★ **页内不含其它客户数据** | `TC-M9-05`：造**两个客户**各一批，生成 A 的报告 ⇒ 断言快照**不含** B 的客户名 / 成品批码 / 袋码 / 生产批码 |
| A7 | ★★ **快照受白名单约束** | `TC-M9-06`：断言快照**不含**内部字段（`remark` 内容 / `operator` 值 / `urgent_release` / `void` 留痕 / 审计字段） |
| A8 | ★★★ **让步不披露** | `TC-M9-08`：用**让步接收料**产出成品批并出报告 ⇒ 断言快照**不含**「让步」/「CONCESSION」/「让步接收」；★ 同时断言**同批的内部档案** `GetBatchArchive(...).ConcessionUsed == true`（**两者同时成立**） |
| A9 | ★ **访问留日志** | `TC-M9-07`：对 `reportd` 发一次访问 ⇒ 跑 `access/sync` ⇒ 断言 `b_share_access` 有该行（`report_id` / `accessed_at` / `ip` / `ua`）；★ 再跑一次 sync ⇒ **不新增重复行**（幂等） |
| A10 | ★★ **公网侧不连内网库** | ★ 机检：`go list -deps ./cmd/reportd` **不含** `database/sql` / `go-sql-driver/mysql`；★ 且 `reportd` **不读** `JX_DB_DSN`；★ 并**真跑**：停掉 MySQL（或用一个错误 DSN 的 `reportd` 环境）后 `reportd` **仍能**正常 200 / 404 —— 反证它没连库 |
| A11 | ★ **`report_no` 取号** | 读实现：`GET_LOCK` ＋**同一事务**内取 max+1；**无**「锁外先查 max 再 +1」；超 999 ⇒ **明确报错**、不进位 |
| A12 | ★ **`token` 不可猜测** | 读实现：`crypto/rand`（**不得** `math/rand` / 时间戳 / 序号派生）；`uk_report_token` 冲突重试；★ 连生成 5 次 ⇒ token 互不相同（用例内断言） |
| A13 | ★★ **M10 只读（零写业务表）** | `TC-M10-01`：**结构化机检**（★ 别用裸词 —— 见 §6-9）：M10 相关文件（`internal/store/rpt*.go` · `internal/httpapi/rpt*.go`）中，**写 `b_*` / `m_*` 表的 `INSERT` / `UPDATE` / `DELETE` 零命中**；★ 允许的唯一写是 `s_audit_log`（导出审计） |
| A14 | ★ 4 个点**真在路由层被消费** ＋ 级别正确 | `check_perm_registry.py` 绿；★ 读路由代码确认不是只在 `all.go` 挂名；★ 级别逐个核对：`report.generate` / `report.share.manage` / `rpt.export` ⇒ **`LevelAll`**、**全部读入口 ⇒ `LevelRead`**，且读入口**只能挂 `rpt.view`**（★ `report.generate` / `report.share.manage` **无 `READ`**） |
| A15 | ★★ **空数据 ≠ 0** | `TC-M10-03`：清空某数据源 ⇒ 断言返回体 **`has_data:false`** ＋ `note == "数据未接入"`；★ 再保有数据但用**不可能命中的筛选** ⇒ 断言 **`has_data:true` ＋ `rows` 空 ＋ `note == "本条件下无记录"`**（★ 两态**文案必须不同**） |
| A16 | **M9 / M10 的 12 条 TC 均有自动化测试** | `TC-M9-01~08` ＋ `TC-M10-01~04` 逐条对得上测试函数（测试名带 TC 编号，形如 `TestTC_M9_01_…`；沿用批 4~批 7 的 **store ＋ httpapi 双层**形态） |
| A17 | ★ **导出**：CSV 内容与屏幕一致 ＋ 有审计 | `TC-M10-04`：导出后断言 CSV 表头/行数与接口返回一致、BOM 存在；★ 且 `s_audit_log` 有 `action='rpt_export'` 一笔 |
| A18 | ★ **服务器真跑全套 TC 全绿** | `bash scripts/run_tc_server.sh`（★ 包集**自动发现**，输出会先打印「包集（N 个）」）；回执须写明**实际包数**与**各包 PASS/FAIL/SKIP**；★ 新增的报表/报告用例须在**同一套**里跑到 |
| A19 | ★ 服务真起且版本一致 | `bash scripts/deploy-test-server.sh --restart --smoke` ⇒ 主服务 `/healthz` **200** 且 `version == HEAD`、**`reportd` `/healthz` 200**；★ 两进程**均只绑回环** |
| A20 | **不动冻结件** | `git diff --stat <基线>..HEAD` 中 `spec/` · `docs/` · `migrations/` **改动文件数 = 0**；权限点仍 **51 × 6 = 306** |

★ **单点变异自证（必做，≥2 处）**，例如：
- 快照**把让步字样渲染出来** ⇒ **A8 必须红**；
- 刷新**复用旧 token**（只改内容不新建行）⇒ **A3 必须红**；
- 撤销**只改 `status` 不移文件** ⇒ **A4 必须红**；
- 过期判定 `now > expires_at` 改成 `now >= expires_at`（或按「日」粒度搞错）⇒ **A5 边界必须红**；
- M10 空数据返回 `rows:[]` 且 `has_data=true` ⇒ **A15 必须红**；
- 报告**直接序列化 M8 读模型**（不过白名单）⇒ **A7（乃至 A8）必须红**。

把变异点、红的证据（哪些用例红、哪些保持绿）、还原后的 `sha256` 写进回执。

---

## 5. 明确不做（本批）

| 项 | 为什么 |
|---|---|
| **`scope_type='按车次'` 的报告** | 一期只按批次；`docs/04` `UC-M9-01` 的落点是"选批次" |
| **Caddy / 云厂商对象存储（S3 等）** | ★ 需 sudo 改 `/etc/caddy`（本项目**无免密 sudo**）；且 `U4` 供应商未定 ⇒ 一期以 `cmd/reportd` 承接（**二期**再切） |
| ★ **链接访问隔离 / 登录鉴权 / 防盗链** | `docs/01 §6.6` 用户已定案：**不做隔离、链接可随意转发**（缓解 = 长随机 token ＋ 非零有效期 ＋ 访问日志 ＋ 页内不含其它客户数据） |
| **报告页导出 PDF / 打印样式** | 无 UC 支撑；浏览器打印即可 |
| **报表的图表组件库（ECharts 等）** | ★ 不引重依赖（§6-11）；一期用**表格 ＋ 简单条形**（纯 CSS）即可 |
| **报表的定时快照 / 订阅推送** | 无 UC 支撑 ⇒ 不做 |
| **留样到期自动提醒** | ★ **二期**（`REMAINING.md §4`） |
| **仪器数据直连自动采集** | **二期** |
| **行级 / 列级权限 · 真实飞书回调** | 一期不做 / 沿用 dev 桩 |

★ 本批需要「生产批 / 投料 / 成品批 / 成品袋 / 检测单 / 出货单 / 留样」等**上游对象**才能验报告与报表 —— 允许用**测试夹具直写库**造最小对象（与批 3~批 7 同一手法）；★ **不得**为此改 M3~M8 的实现（**本批无联动修正**；若你认为确需 ⇒ 回执说明并**开议题**）。

---

## 6. 口径要求（易错点，逐条确认）

0. ★★★ **两条硬约束（不可协商）**：
   - **① 系统调试一律在测试服务器（`192.168.10.50`）上进行，不在本机**（`docs/05-环境与调试约定.md`）。本机只编译 / 纯单测 / 静态检查 / 门禁；**要 listen 或要改远端状态的，去服务器**（`scripts/deploy-test-server.sh`）。★ 本批 `reportd` **要 listen** ⇒ 只能在服务器上起与验。
   - **② 只本地提交、不 push**（`git push` 由 WorkBuddy 在独立验收通过后执行）。
1. ★★ **`report_no` 不是追踪码**（我方定案）：`spec/code-rules.json` 的 `object_types` 只有 `A`~`E` 五类，**无 `report` 对象**（本批开工前已核）⇒ 出货单号不适用 27 位规则；定为 **`RP` ＋ `YYMMDD` ＋ `-` ＋ 当日 3 位序号**，`GET_LOCK` ＋ 同事务、**max 999 明确报错**。
2. ★★★ **公网侧不连内网库（`docs/03` 批 8 验收要点第 ④ 条）**：服务快照的进程（`cmd/reportd`）**零 DB 连接**、**零 DSN 读取**、**不 import `internal/store`**。⇒ **有效性判定只能靠"文件在不在 `served/`"** —— 撤销 / 过期 ⇒ **移文件**。★ **不得**让 `reportd` 去查 `b_share_report.status`。
3. ★★★ **快照内容受白名单约束**（`TC-M9-05` / `TC-M9-06` / `TC-M9-08`）：
   - 白名单 = D1 所列各项；**黑名单** = ①「让步」/`CONCESSION` 字样；② 其它客户数据；③ `remark` / 操作人 / 作废留痕 / 紧急放行 / 审计 / 留样 / 班组人员等内部字段。
   - ★ **实现建议**：**专用投影结构体**（白名单由**类型**保证）+ 渲染层只读它；**禁止**把 M8 的 `BatchArchive` 直接序列化。
   - ★ **让步不披露是用户定案（`docs/01 §11.2 D20`）**：★ **内部档案照旧显著标注**（M8 已交付），两者**同时成立**。★ 前端也**不得**硬编码「让步」字样。
4. ★★ **刷新 = 新 token 新链接**：新建行（新 token / 新 report_no / 新快照）＋ **旧行置 `已撤销` ＋ 旧快照移出**。★ 不得改同一行 / 复用同一 token。
5. ★ **撤销单步生效**：`report.share.manage` **一个点** ⇒ **无「发起 / 审批」两段式**（与 M7 出货单撤销**不同**，不要照抄批 7 的 `init`/`approve` 形态）。★ `reason` 必填。
6. ★ **`expires_at` 的语义（我方定案）**：**「到期日（含）」** —— 存 **该日 23:59:59.999**；判定「已过期」 = **`now > expires_at`** ⇒ `TC-M9-04`「有效期末日访问可访问、次日起不可访问」自然成立。★ **缺省有效期 = 生成时刻 + 30 天**（同一语义，取当日 23:59:59.999）。★ 不得把 `NULL` 解释成"永不过期"而不写文档（本批缺省**不产生 NULL**）。
7. ★★ **读入口的权限点归属（易错）**：`report.generate` = `["ALL","NONE"]`、`report.share.manage` = `["ALL","NONE"]` ⇒ **两者都无 `READ`**。⇒ **M9 的全部读入口（报告列表 / 详情 / 访问日志）统一挂 `rpt.view` 的 `LevelRead`**（本批唯一含 `READ` 的点）。★ 判例同批 7（`ship.void.approve` 无 `READ` ⇒ 读入口改挂 `ship.void.init`）。★ 写入口：`report.generate` / `report.share.manage` / `rpt.export` ⇒ **`LevelAll`**。★ **不得**新增权限点、**不得**裸写权限点字符串（用 `internal/permission/code.go` 的常量）。
8. ★★ **访问日志的幂等与不回放**：`(report_id, accessed_at, ip)` 去重；同步进度以 **byte offset**（推荐）或滚动 `access.log.synced`（★ 若滚动，须处理 `reportd` 持有旧 fd 继续追加 ⇒ **会丢行**）。★ 回执里写清**选了哪种、为什么**。
9. ★★ **判据纪律（附加铁律 9）**：凡「在某范围内找某标记」的判据，**必须匹配结构化位置，不得匹配裸词** —— 裸词会被文档里的「讨论」命中而**恒真**。★ 尤其 `A13`（M10 零写）与 `A10`（reportd 零 DB）这类**机检**：要**限定到本批新增文件**、或按 **SQL 关键字 ＋ 表名**结构化匹配，或按**依赖闭包**（`go list -deps`）判 —— **不得**用「有没有 `UPDATE` 这个词」。★ 判据自身必须先被探针打过（真阳性保留 / 假阳性消除 / 旧逻辑复现）。
10. ★★ **heredoc 纪律（附加铁律 10）**：`<<EOF` **不加引号**时，体内**反引号与未转义的 `$` 会在本机被执行/展开**。⇒ 要发给远端的脚本，**注释里一律用「」，不要用反引号**；需要远端展开的 `$` 写 `\$`。★ **引号纪律**（原拟附加铁律 13，本轮**仍不新立**，但**请照做**）：凡把**可能含空白或换行**的文本当**命令参数 / 重定向目标**，**必须加引号**（2026-10-09 曾因此产生 16 个 0 字节乱名文件）。
11. ★ **不加 `cgo`、不加需要联网下载的重依赖**；★ **不要引前端图表库**（一期表格 ＋ CSS 条形即可）。如需新增依赖，**先在回执里说明理由**。
12. ★ **`s_audit_log` 的写法**：`appendAuditTx`（`internal/store/audit.go`）已交付；`action` 为 `VARCHAR(32)` ⇒ `report_generate`(15) · `report_refresh_supersede`(24) · `report_set_expires`(18) · `report_revoke`(13) · `report_expire`(13) · `rpt_export`(10) **长度均合规**。
13. ★ **测试夹具纪律**（批 3 起沿用）：跨包共用测试库时，清理必须**按账号精确匹配**（禁用前缀通配）、**先删子表再删父表**；断言「某表为空」必须**限定本用例作用域**，不能全库 `COUNT(*)`。★ 本批父表 `b_share_report` ／ 子表 `b_share_access` ⇒ **先删 access 再删 report**；★ 还要**清理 `JX_REPORT_DIR` 下本用例产生的文件**（否则跨用例互相污染）。
14. ★★ **写 `COLLAB.md` 只能用「局部追加」** —— **禁止整体重写，禁止用格式化工具把该文件整体写回**（已出事故：底本是旧版本 ⇒ 冲掉对方内容）。门禁 `scripts/check_collab_anchors.py` 会判红这类覆盖。★ **md 表格里不要写裸 `|`**（会被当成列分隔符 ⇒ `check_md_tables.py` 判红；写 `INSERT / UPDATE / DELETE` 而不是 `INSERT|UPDATE`）。
15. ★ **空数据 ≠ 0 的同族判据**（我方重申）：`TC-M8-03`「无流向 ≠ 报错」、`TC-M10-03`「无数据显示『数据未接入』，不是 0」—— ★ **"错误表现为正确"是本项目明文禁止的反模式**。
16. ★ **不得**为凑进度自造任务；★ **不得**跳过/注释掉/"改成期望"任何失败测试。
17. ★ 若发现**本批过大**（M9 ＋ M10 ＋ 新进程 ＋ 日志管道），**回执说明并建议拆分**（`docs/03 §3-5` 与 `COLLAB.md §2` 允许），**不要硬做完、也不要闷头做一半**。
18. ★ 若发现**规格本身有问题**（`spec/` 或 `docs/04` 的 UC/TC 有矛盾），**开议题**，**不要自己改规格**（判例：`N-009`）。

---

## 7. 完成后（三条全满足）

1. `COLLAB.md` 中 **`N-015` 段**写回执 ＋ 状态改 `MIMO-DONE`（★ 必须是**行首恰为 `- **状态**：MIMO-DONE`** 的那种行 —— 驱动的判据① 只认这个结构化位置）；
2. **提交代码**（显式路径，**禁 `git add -A`**）；★ **按附加铁律 12 分阶段提交**（后端一块 / 静态服务一块 / 服务器 TC 全绿 / 变异自证 / 前端，各提交一次）；
3. 提交前 `bash scripts/check_all.sh` **全绿**，并在服务器跑 `bash scripts/run_tc_server.sh`（★ 输出会先打印「包集（N 个）」—— 请把**实际包数**与各包结果写进回执），另跑 `bash scripts/deploy-test-server.sh --restart --smoke`（★ 主服务 ＋ `reportd` 两个 `/healthz`）。

★ **回执请尽早落盘**：批 7 的教训是"做完了但回执没写就被回收"。⇒ **建议每完成一块就把回执草稿 `git add COLLAB.md && git commit`**（局部追加，不整体重写）。

---

## 8. 本批我方（WorkBuddy）已先行指出的易错点汇总

| # | 易错点 | 落在哪条 |
|---|---|---|
| 1 | ★★★ 报告里**披露了让步接收**（`D20` 明令对外不披露） | §1-1 / §6-3 / A8 |
| 2 | ★★★ `reportd` **连了数据库**（或读了 DSN）⇒ 违反「公网侧不连内网库」 | §1-2 / §6-2 / A10 |
| 3 | 撤销/过期**只改 `status`、不移快照文件** ⇒ 旧链接仍能打开 | §6-2 / A4 / A5 |
| 4 | 刷新**改同一行 / 复用同一 token** ⇒ 「同一 URL 内容悄悄变」 | §1-3 / §6-4 / A3 |
| 5 | 快照**直接序列化 M8 读模型**（把 `remark` / 操作人 / 紧急放行 / 作废留痕带出去） | §6-3 / A7 |
| 6 | 快照**掺入其它客户的数据** | §6-3 / A6 |
| 7 | **读入口挂 `report.generate` / `report.share.manage`**（两者**无 `READ`**）⇒ 谁都读不到 | §6-7 / A14 |
| 8 | 撤销**照抄批 7 的「发起 / 审批」两段式**（本批是单步生效） | §6-5 |
| 9 | `expires_at` 语义搞错（把 `NULL` 当"永不过期"、或把边界做成"末日就失效"） | §6-6 / A5 |
| 10 | `token` 用**时间戳 / 序号 / `math/rand`** ⇒ 链接可枚举 | A12 |
| 11 | 文件名用 **`report_no`** 而不是 token ⇒ 链接可枚举 | D1 |
| 12 | 取号写成「锁外先查 max 再 +1」 | §6-1 / A11 |
| 13 | M10 **顺手写了业务表**（或用裸词机检 `A13` ⇒ 判据恒真） | §6-9 / A13 |
| 14 | M10 **无数据返回 0 / 空数组**且不给 `has_data` / 两态文案相同 | §1-4 / §6-15 / A15 |
| 15 | 访问日志 sync **不幂等**（重复跑产生重复行）或**回放**（重复计） | §6-8 / A9 |
| 16 | 日志滚动用 `rename` 但 `reportd` **持有旧 fd 继续写** ⇒ **丢行** | §6-8 |
| 17 | 去改 **`/etc/caddy`**（无免密 sudo，改不了也不该改） | §5 / D7 |
| 18 | **回执留到最后写**，结果被整树回收 ⇒ 状态卡 `OPEN`（批 7 实录） | §0-C / §7 |
| 19 | md 表格里写**裸竖线**（如 `INSERT` 与 `UPDATE` 之间）⇒ `check_md_tables.py` 判红（本轮 WorkBuddy 自己踩过两次） | §6-14 |
| 20 | 前端**硬编码「让步」字样**，或在报表用**绿**表示不合格 | §6-3 / D6 |
