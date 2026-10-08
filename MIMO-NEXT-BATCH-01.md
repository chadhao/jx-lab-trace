# MIMO-NEXT-BATCH-01 · M0 地基

> **交付方**：mimo code ｜ **验收方**：WorkBuddy｜ **派出日**：2026-10-08
> **批次**：批 1 / 8 ｜ **模块**：**M0 地基** ｜ **依赖**：无（本批是其余 7 批的地基）
> ★ **开工前必须完整读 `MIMO-ONBOARDING.md`**（含铁律与禁止事项），再读本文件。

---

## 1. 本批目标（一句话）

**把"后面 10 个模块有地方可写"这件事做完**：仓库骨架 · 配置 · MySQL 迁移 · 飞书免登 · 会话落库 · **权限引擎** · 审计 · **门禁跑通**。
★ **本批不含任何业务单据**（不做收货、不做检测、不做码）。

---

## 2. 必读

| # | 文件 | 读什么 |
|---|---|---|
| 1 | `COLLAB.md` | §0 铁律 · §1 当前状态 · §2 责任域 |
| 2 | `docs/01-设计定案.md` | §2 **四条设计原则** · §5 数据模型 · §8 **权限模型与配置** · §10 技术约定 |
| 3 | `docs/03-模块划分与实施批次.md` | 批 1 的验收要点 · §3 推进规则 |
| 4 | `docs/04-模块设计与用例.md` | **M0 全部 UC 与 TC**（这是你的验收标准） |
| 5 | `spec/schema.sql` | 38 张表（★ 本批要把它变成可重复执行的迁移） |
| 6 | `spec/permission-points.json` | 51 个权限点 · 6 个角色 · 授权种子 · `constraints` |

---

## 3. 交付物

### D1 · 仓库骨架

- Go module（`go 1.24`+）· `cmd/jxlabtrace/` 主入口 · `internal/` 分层（`config` `store` `httpapi` `permission` `access` `audit` `codec` `webui`）· `migrations/` · `web/`（Vue3+Vite，构建产物 `//go:embed` 进二进制）· `scripts/`
- `scripts/build.sh`：`vite build` → `cp web/dist` → `go build -trimpath -ldflags "-s -w -X main.version=..." -o bin/jxlabtrace ./cmd/jxlabtrace`

### D2 · 配置

从环境变量读（**一律有安全缺省或明确拒绝启动**）：

| 变量 | 用途 | 缺省 |
|---|---|---|
| `JX_DB_DSN` | MySQL DSN | **无缺省 ⇒ 必须显式给，否则拒绝启动** |
| `JX_DEV_MODE` | 开发模式（免登桩） | `false` |
| `JX_DEV_OPEN_ID` | dev 模式固定身份 | 空 |
| `JX_FEISHU_APP_ID` / `JX_FEISHU_APP_SECRET` | 真实飞书免登 | 空 |
| `JX_SESSION_TTL` | 会话时长 | `12h` |
| `JX_BOOTSTRAP_SYS_ADMIN_OPEN_ID` | 首个系统管理员引导 | 空 |
| `JX_HTTP_ADDR` | 监听地址 | `127.0.0.1:8080` |
| `JX_ATTACH_DIR` | 附件目录 | `/srv/jx-lab-trace/attachments` |

★ **改配置后强制回读校验**（启动日志里回显生效值，**密码类不打明文**）。

### D3 · MySQL 连接与迁移

- 驱动：**纯 Go**（`github.com/go-sql-driver/mysql`），**不引入 cgo**。
- 迁移：**可重复执行（幂等）**；表结构以 `spec/schema.sql` 为准（**可以调索引/字符集/注释，但列的语义不得改**）。
- 迁移同时种入：**51 个权限点** + **6 个角色** + **`spec/permission-points.json` 的完整 `seed` 授权矩阵**。
- 提供 `-migrate` 子命令或启动时自动迁移（二选一，回执里说明）。

### D4 · 飞书免登

- **真实模式**：授权 → 回调 → 换 `open_id` → 查角色 → 建会话。
- **dev 模式（本批必须有）**：用 `JX_DEV_OPEN_ID` 直接建会话，跳过飞书（因为**用户尚未提供飞书应用凭据**，见 `REMAINING.md#U1`）。
- ★ **未映射任何角色 ⇒ 拒绝**（deny by default；**不是"默认放行"**）。

### D5 · 会话（★ 本批最容易做错的一项）

- **必须落库**（`s_session`），**不得用内存 map**。
- **滑动续期**：每次访问续 `expires_at`，且 ★★ **cookie 的 `Max-Age` 必须与服务端 `expires_at` 同批续** —— 否则浏览器侧先过期、服务端以为有效，仍然掉线。
- cookie：`HttpOnly` + `SameSite=Lax`（生产加 `Secure`）。
- **登出立即失效**，且**重启后仍失效**。

### D6 · 权限引擎

- **权限点由代码注册**（常量 + 启动时 upsert 进 `s_permission_point`）；**后台不可增删**。
- 判定：`角色 → 权限点 → 级别`；**多角色取并集**；无记录 ⇒ `NONE`。
- **每个受保护入口必须引用已登记的权限点常量**，**禁止裸写字符串**。
- 提供最小可用入口以证明引擎能用：`GET /healthz`（免权限）· `GET /api/me`（返回 `open_id`/`name`/`roles`/`sys_roles`）· 至少 **1 个受权限保护的示例入口**（例如 `GET /api/admin/permission-points`，受 `sys.perm.edit`… 或任意一个你选的点，回执里说明）。

### D7 · 审计

- 写操作统一入口落 `s_audit_log`：`entity / entity_id / action / field / old_value / new_value / actor_open_id / actor_name / actor_role / ip / reason / at`。
- ★ **只增不改**。
- 本批至少让"权限相关的写操作"走通审计（批 2 的矩阵页要用）。

### D8 · 门禁（★ 本批的硬性交付）

`scripts/check_all.sh`，**必绿 / 会报 分列**（依设计定案与既有项目教训：**把"会报"并进"必绿"会让红成为常态、进而被忽略**）：

**必绿**（任一失败 ⇒ 总判定失败）：
1. `gofmt -l .`（**输出为空**才算过；排除 `_` 前缀目录，对齐 `go build` 口径）
2. `go build ./...`
3. `go vet ./...`
4. `go test ./... -count=1`
5. `python scripts/check_md_tables.py` —— **已就位**
6. `python scripts/check_collab.py` —— **已就位**
7. `python scripts/check_uc_tc.py` —— **已就位**（用例↔测试用例覆盖门禁，由 WorkBuddy 实现）
8. `python spec/verify_code_rules.py` —— **已就位**
9. `python spec/verify_permission_points.py` —— **已就位**
10. ★ **`python scripts/check_perm_registry.py`** —— **本批你要实现**（见下）

**会报**（不阻塞）：`python scripts/check_md_structure.py`（已就位）· 你自选的静默缺陷排查（可选）

★ **`scripts/check_perm_registry.py` 要实现的判据**（贯彻「声明了却没人执行 ＞ 没声明」这条铁律）：
- **① 消费端**：`spec/permission-points.json` 里的**每一个** `code`，必须在 Go 非测试代码中**至少被引用一次**（以权限点常量或字符串字面量形式）—— 防「**配了没人用**」的假权限；
- **② 声明端**：Go 代码中出现的、**所有**受保护入口引用的权限点标识，必须在 `spec/permission-points.json` 里**已登记** —— 防「**没声明却保护了**」；
- **③ 禁裸写**：受保护入口处**不得**直接写权限点字符串字面量，必须引用**常量**（本项可用"常量定义集中在单一文件"来近似实现，回执里说明你的做法）；
- **④ 种子一致**：`spec/permission-points.json` 的 `seed` 与库内 `s_role_permission` 行数一致（51 × 6 = **306**）；
- 退出码表达失败；把它加进 `scripts/check_all.sh` 的**必绿**段。

★ 提示：② 与 ③ 的实现在"如何识别受保护入口"上需要你选一个方案（AST 扫描 / 集中注册表 / 代码生成）。**任选其一，但必须在回执里写清判据边界**（你能保证什么、不能保证什么）——
  ★ **不许**把它做成"看起来在查、实际查不到"的假判据。

---

## 4. 验收判据（WorkBuddy 会逐条独立复核）

| # | 判据 | 怎么验 |
|---|---|---|
| A1 | `bash scripts/check_all.sh` **必绿全绿** | 复跑 |
| A2 | 迁移**幂等** | 连跑两次，`information_schema` 表数稳定 = **38**；第二次无报错 |
| A3 | dev 模式可登入 | 实测 |
| A4 | ★★ **会话跨重启有效** | 登入 → **重启进程** → 用**原 cookie** 访问 ⇒ 仍有效 |
| A5 | ★ **登出后重启仍失效** | 登出 → 重启 → 原 cookie ⇒ 失效 |
| A6 | ★ **未映射角色被拒** | 用一个未映射 `open_id` 访问受保护入口 ⇒ **拒绝** |
| A7 | 权限点注册 | 库内权限点 = **51**、角色 = **6**、授权行数 = 51×6 = **306** |
| A8 | ★ **后台不能新增权限点** | 直接调相关接口/或不存在该接口 ⇒ 确认无此通路 |
| A9 | ★ **每个权限点至少被一处代码消费** | `check_all.sh` 里的判据报红测试（你实现） |
| A10 | 审计有记录 | 做一次权限相关写操作 ⇒ `s_audit_log` 有行 |
| A11 | **M0 的 TC 全过** | `TC-M0-01` … `TC-M0-09` 各有对应自动化测试 |

★ **单点变异自证（必做）**：至少做 **2 处**，例如
- 把会话存储改成**内存 map** ⇒ **A4 必红**；
- 把某权限点判定改成"无记录即放行" ⇒ **A6 必红**。
把变异点、红的证据、还原后的 `sha256` 写进回执。

---

## 5. 明确不做（本批）

| 项 | 为什么 |
|---|---|
| **码引擎**（生成/解析/校验位） | **批 3**（M3） |
| 任何业务单据与页面 | 批 2 起 |
| 真实飞书回调 | 用户未给凭据（`REMAINING.md#U1`）；本批只做 **dev 桩** + 预留 |
| 前端页面（除最小可用） | 批 2 起 |
| 报表 / 报告 / 追溯 | 批 7–8 |

---

## 6. 口径要求（易错点，逐条确认）

1. ★ 表结构以 `spec/schema.sql` 为准 —— **列语义不得改**；要改 ⇒ **开议题**。
2. ★ 权限点 `code` 以 `spec/permission-points.json` 为准，**51 条一一对应**，且**每个点至少被一处代码消费**（否则门禁红）。
3. ★ **`s_user_role` 用 `UNIQUE(open_id, role_code)`**，不是 `open_id` 单列唯一 —— 一账号可叠加多角色。
4. ★ **`m_user` / `m_team` 不带版本链**（人员与班组的来去不是"版本"语义）。
5. ★ **不加 `cgo`、不加需要联网下载的重依赖**；如需新增依赖，**先在回执里说明理由**。
6. ★ 本地仓库**暂无远端**，**不要 push**；只提交到本地 `main`。

---

## 7. 完成后（三条全满足）

1. `COLLAB.md` 中**本批议题段**写回执 + 状态改 `MIMO-DONE`；
2. **提交代码**（显式路径，**禁 `git add -A`**）；
3. 提交前 `bash scripts/check_all.sh` **全绿**。

★ 若中途发现**本批过大**，**回执说明并建议拆分**（`COLLAB.md §2` 允许），**不要硬做完、也不要闷头做一半**。
★ 若发现**规格本身有问题**（`spec/` 或 `docs/04` 的 UC/TC 有矛盾），**开议题**，**不要自己改规格**。
