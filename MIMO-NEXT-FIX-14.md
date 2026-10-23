# MIMO-NEXT-FIX-14 —— `/api/me` 增加 `permissions` 聚合字段（N-030，APP 权限矩阵驱动）

> 交付方：**mimo code**　提出方：APP 设计侧（`BACKEND-REQ-07.md`，高优——APP 首页入口权限判定依赖），经 WorkBuddy 转达
> ★ 项目方裁决：**前端权限判定按权限矩阵**——首页入口可见性＝用户全部角色在矩阵中聚合出的权限点，**不做任何角色名特判**（APP 将删除现存的 sysadmin 过渡特判）。

## D1 · `GET /api/me` 增加 `permissions: string[]`

- **聚合口径**：该用户全部角色在 `s_role_permission` 的权限点 code **并集（去重）**。
- ★★ **口径裁定（必须回执注明）**：**与 `RolesOf` 一致——仅聚合「启用」角色**（`s_role.status='启用'`）。理由：停用角色的授权不应继续生效，与权限判定的既有口径一致。（APP 文档的参考 SQL 未过滤 status，属其示例简化——后端自裁权在本侧。）
- ★ **列名以实际 schema 为准**（`s_role_permission.point_code`——APP 文档示例写的 `perm_code` 是其简化）。
- ★ 输出**排序稳定**（按 code 字典序）——契约确定性与测试友好。
- ★ **实现**：新增只读 store 方法（如 `PermissionCodesFor(ctx, openID)`），复用 RolesOf 的 JOIN 模式；`/api/me` handler 挂上即可。**不改矩阵、不新增权限点**。

## D2 · 兼容性红线

1. **仅新增字段**：`open_id/name/roles/sys_roles` 原样（既有字段回归 TC 钉住）。
2. **Web 端零影响**：多出的字段对 Web 前端无影响——既有 `/api/me` 用例回归钉住。
3. 无角色用户 ⇒ `permissions` 为**空数组 `[]`**（**非 null**——APP 侧判定 `∈` 语义需要数组）。

## D3 · 测试（TC 至少 5 条）

1. **sysadmin 会话** ⇒ `permissions` 含**全部 51 点**
2. **仅 production 角色** ⇒ 生产域子集（`sample.take`/`prod.feed.scan` ∈；`ship.load.scan`/`rpt.view` ∉——按矩阵现值断言）
3. **停用角色口径**：用户含一停用角色＋一启用角色 ⇒ 仅聚合启用角色的点（口径钉住）
4. **无角色** ⇒ `"permissions":[]`（非 null）
5. **形状回归**：`open_id/name/roles/sys_roles` 原样 ＋ Web 端既有用例全绿

## D5 · 交付

- ★ 门禁 **11/11**；部署测试服务器（`.env` 红线、healthz＝提交无 dirty）；**curl 实测**：sysadmin 51 点／production 子集／空数组三对照。
- ★ 回执写 `COLLAB.md` **`### N-030` 段内**、状态 **`MIMO-DONE`**（**替换**）；显式路径提交；只本地提交不 push；**口径声明**（启用角色）必写。

## §6 口径要求

1. ★ **不新增权限点、不改矩阵、不改 RolesOf**——只加只读聚合。
2. ★ 其余同 FIX-02 §6（服务器验收 / 引号纪律 / 时间戳 / COLLAB 局部追加 / 部署用已提交 commit）。
