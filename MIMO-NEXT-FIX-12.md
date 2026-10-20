# MIMO-NEXT-FIX-12 —— exchange 移动端（SDK）token 契约分支 ＋ 拒绝路径日志（N-028，P0）

> 交付方：**mimo code**　提出方：APP 设计侧（`BACKEND-REQ-05.md` 合并 `REQ-04`，P0），经 WorkBuddy 转达
> ★ 根因（官方文档证据）：exchange 用**网页 OAuth v2 端点（带 redirect_uri）**处理了**移动端 SDK 的 code**；官方「app-login/get-access_token」明确：**移动端登录 → `passport.feishu.cn/suite/passport/oauth/token`、不传 redirect_uri**。后果：换出的 open_id 与移动端授权语义错位 ⇒ `AUTH_NO_ROLE`（绑定/通讯录/映射均已排查排除）。

## D1 · `handleFeishuExchange` 增加来源分支

- 请求体加 **`"source":"sdk"`**（APP 显式声明；空/其它 = 网页流，行为不变）。
- **`source=="sdk"` 分支**：
  - token 端点：**`https://passport.feishu.cn/suite/passport/oauth/token`**
  - 参数：`grant_type=authorization_code, client_id, client_secret, code` —— ★ **不传 redirect_uri**
  - **挑战码（code_verifier）为 P1 后续扩展**，本次不做
- **`source` 空/其它**：保持现行为（v2 端点 ＋ redirect_uri）——**callback 流程兼容性红线**
- ★ 实现建议：`internal/access/feishu.go` 增加 `ExchangeSDK(ctx, code)`（或给 Exchange 加来源参数），**token 响应解析注意 passport 端点与 v2 的返回形状差异**（以官方 app-login 文档为准，两种都兼容处理或按分支分别解析）。
- ★ **user_info 流程不变**：`/open-apis/authen/v1/user_info` 两种 token 均应兼容；**若实测不兼容**，改用官方移动端登录文档指定的用户信息端点，并回执说明。

## D2 · 拒绝路径日志（原 REQ-04，排查实锤手段）

- `ErrLoginDenied` 分支加：
  ```go
  c.Logger().Warn("exchange 拒绝: open_id=", ident.OpenID, " name=", name)
  ```
- ★ 响应结构/流程**一字不改**，仅诊断日志。

## D3 · 兼容性红线（回归必测）

1. **现有 `GET /feishu/callback` 一字不改**（TC 钉住）
2. **dev 桩登录不受影响**
3. **exchange 现有三错误分支语义不变**（401/403 userMessage/502——REQ-03 契约）
4. **首登 bootstrap 照常触发**（`source=sdk` 的 feishu_login 同样走 openSession/bootstrap 链——清库后 APP 首登场景）

## D4 · 测试（★ 无法构造真实 SDK code ⇒ 分层验证）

1. **单测**：SDK 分支的**请求构造断言**（端点 URL＝passport、参数**不含 redirect_uri**、含 grant_type/client_id/client_secret/code）——mock HTTP 逐字段
2. **单测**：passport 端点响应形状解析（按官方文档示例向量构造）
3. **单测**：`source` 空 ⇒ 走 v2（回归）；`ErrLoginDenied` ⇒ Warn 日志含 open_id
4. **服务器**：部署后 curl `exchange`（假 code）⇒ 错误分支正常（回归）；callback/dev 回归
5. ★ **真机 SDK code 的最终验证由用户/APP 侧执行**（部署后 APP 重试登录 ⇒ 200＋Set-Cookie 即实锤）——**如实声明为本交付的未做项**

## D5 · 交付

- ★ 门禁 **11/11**；部署测试服务器（`.env` 红线、healthz＝提交无 dirty）；curl 回归（假 code 分支＋callback 302＋dev 登录 403 未映射）。
- ★ 回执写 `COLLAB.md` **`### N-028` 段内**、状态 **`MIMO-DONE`**（**替换**）；显式路径提交；只本地提交不 push；**如实声明**：真机 SDK code 端到端验证待 APP 重试（部署后即解锁）。

## §6 口径要求

1. ★ **callback/dev/exchange 错误语义一字不改**（三处兼容红线）；只加分支与日志。
2. ★ **不新增权限点**；exchange 免权限不变。
3. ★ 其余同 FIX-02 §6（服务器验收 / 引号纪律 / 时间戳 / COLLAB 局部追加 / 部署用已提交 commit）。
