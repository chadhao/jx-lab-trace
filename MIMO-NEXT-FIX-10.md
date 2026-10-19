# MIMO-NEXT-FIX-10 —— 取样权限职责更正：sample.take 由 qc 移交 production（N-026）

> 交付方：**mimo code**　提出方：APP 设计侧（`jx-lab-trace-app/docs/BACKEND-REQ-01.md`），经 WorkBuddy 转达　对应议题：**`N-026`**
> ★ 业务事实更正：**取样由生产班组完成，样品再交质检检测**（不是质检取样）。APP 端将把取样入口放生产班组名下；后端授权现状不改则 APP 调用 403。

## D1 · 真相源：`spec/permission-points.json`

- 第 107 行 `seed["sample.take"]` **仅改两个值**：`qc: ALL → READ`；`production: NONE → ALL`。其余角色（receiver/management/sales/sysadmin）**不动**。
- ★ 语义：qc 保留样品列表读取（READ）；取样写权限归 production；**这正是职责分离要的审计效果**（`sampled_by` 将记录生产班组的人）。
- ★ **其它权限点一律不动**（`sample.retain.*` 保持 `qc: ALL`——APP 不实现这两个动作）。

## D2 · 既有测试同步（★ 权限变更的必然波及，必须处理否则门禁红）

- 排查既有用例中「**qc 身份取样**」的测试（如 m3/sample 相关 TC）⇒ **改用 production 身份**（断言不变，角色变）。
- **新增/调整 TC 对应 BACKEND-REQ-01 §四 验收 5 条**：
  1. production 身份 `POST /api/sample/take` ⇒ 200，`sampled_by` 为该生产账号
  2. qc 身份 `POST /api/sample/take` ⇒ **403**
  3. qc 身份 `GET /api/sample/samples` ⇒ 200
  4. 重启后 `GET /api/admin/permission-matrix` ⇒ `sample.take: production=ALL, qc=READ`
  5. **全新空库跑迁移** ⇒ 种子落库即新值（证明 spec 生效）

## D3 · 运行时生效双路径（★ 缺一不可，BACKEND-REQ-01 §2.2 指出的坑）

- **路径 A（spec，mimo 做）**：改 seed ⇒ **全新空库迁移**即落新值。
- **路径 B（运行库，部署后由 WorkBuddy 执行）**：现有库行已存在，`INSERT IGNORE` 不覆盖 ⇒ **WorkBuddy 将走 `PUT /api/admin/permission-matrix` 原子保存**（有 diff 审计与防锁死校验）。
- ★ **不要在代码/脚本里写死 SQL UPDATE 迁移**——运行库由运维走后台接口（有审计），与需求文档推荐一致。

## D5 · 交付

- ★ 门禁 **11/11**；部署测试服务器（`.env` 红线、版本＝提交无 dirty）；**curl 实测验收 1–3**（验收 4–5 由部署与空库迁移机制背书，WorkBuddy 复核矩阵 API）。
- ★ 回执写 `COLLAB.md` **`### N-026` 段内**、状态 **`MIMO-DONE`**（**替换**）；显式路径提交；只本地提交不 push；如实声明未做项。

## §6 口径要求

1. ★ **只动 `sample.take` 一个点的两个值**；其余权限点/角色/矩阵一律不碰。
2. ★ **不新增权限点、不新增表/接口**。
3. ★ 其余同 FIX-02 §6（服务器验收 / 引号纪律 / 时间戳 / COLLAB 局部追加 / 部署用已提交 commit）。
