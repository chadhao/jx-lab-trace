# MIMO-NEXT-FIX-02 —— 采用期系统配套（2026-10-12）

> 交付方：**mimo code**　提出方：WorkBuddy　对应议题：**`N-018`**
> ★ 背景：一期 8 批已 `AGREED`；`docs/21-采用与验证计划.md` 定案了上线路径与系统侧待命清单。**用户已确认启动其中两项**（飞书组织同步、登录体验变更）。本单是**修复/配套单**，不新开批次。

---

## D1 · 【变更】登录被拒的**友好页面**（替代裸 JSON）

**现状（实测）**：未分配角色的真实用户走飞书登录，浏览器看到的是**裸 JSON**：
```json
{"error":"未映射任何角色，拒绝登录","hint":"deny by default：s_user_role 中无该 open_id 的绑定","open_id":"ou_..."}
```
**要求**：
1. ★ `handleFeishuCallback` 在**拒绝**（deny/缺 code/换 token 失败）时，返回一个**极简服务端渲染 HTML 错误页**（含：人话原因、「联系管理员」、一个指回 `/` 的链接），**不再返回裸 JSON**。
2. ★ 该 HTML 页**不依赖前端路由/JS**（它是新用户看到的第一屏，必须独立成立）。
3. ★ `handleDevLogin` 的拒绝**保持 JSON 不变**（那是给开发/测试用的）。

**验收**：curl 直打回调（无 code / 伪造 code）⇒ `Content-Type: text/html` 且页面含「联系管理员」；带合法会话的正常路径不受影响。

---

## D2 · 【变更】「开发模式免登」入口**按 dev 标志显隐**

**现状**：登录卡（`web/src/App.vue`）与右上角的 dev 登录按钮**无条件显示** ⇒ 双轨/生产期会暴露。
**要求**：★ 启动时 `GET /healthz`（已返回 `{"dev":true,...}`）⇒ **`dev=false` 时隐藏全部 dev 入口**（登录卡按钮 + 右上角），`dev=true` 时照旧。
**验收**：`healthz.dev=false` 环境下，页面源码/渲染结果中**不出现**「开发模式免登 / dev 登录」；`dev=true` 环境照旧。

---

## D3 · 【新增】飞书组织同步（`U10`，`docs/21 §5`）

**定案口径（`docs/21 §4`）**：手动同步按钮 ＋ 后台事件回调；**同步只建/更新账号，绝不自动绑角色**；未分配角色 ⇒ 登录提示「联系管理员」。

### D3.1 飞书客户端扩展（`internal/access/feishu.go`）
- ★ `TenantAccessToken()`：`POST https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal`（app_id+app_secret）—— ★ 端点与响应字段**以官方文档为准，实现前先核对**。
- ★ `ListUsersByDepartment(departmentID)`：`GET /open-apis/contact/v3/users/find_by_department`（**tenant_access_token**，`department_id_type=open_department_id`，分页 `page_token/page_size`）。
  ★ 官方口径：`department_id=0`＝根部门（需**全员**通讯录权限范围）；**全公司 = 先「获取子部门列表」（`fetch_child=true`）再逐部门拉用户**。

### D3.2 同步服务与接口
- ★ `POST /api/admin/org/sync`（管理域，**复用现有 `sys.*` 权限点**——**不新增权限点**，51 点冻结；选哪个点、为什么，**在回执里说明**）。
- ★ 行为：递归拉部门 → 逐部门拉用户 → **upsert `s_user`（open_id 唯一键）**；**只建/更新，不删除、不动 `s_user_role`**。
- ★ 幂等：重复调用结果一致；**返回统计**（新增/更新/跳过）。
- ★ 配置：无需新增（复用 `JX_FEISHU_APP_ID/SECRET`）。

**验收**：单测用 **httptest 假飞书**（假 token 接口 + 假通讯录接口，**形状断言**：Authorization=Bearer、分页参数、upsert 幂等）；**只 upsert 不删除**有测试钉住。

---

## D4 · 【新增】飞书事件回调（入职建号 / 离职停用）

### D4.1 回调端点
- ★ `POST /feishu/event`（**免权限**，飞书服务器调用）：① URL 校验 challenge 应答；② **验签**（`X-Lark-Signature`，HMAC-SHA256，key = `JX_FEISHU_ENCRYPT_KEY`）；③ 解析事件。
- ★ 配置进 `.env.deploy.example`：`JX_FEISHU_ENCRYPT_KEY=""`、`JX_FEISHU_VERIFICATION_TOKEN=""`（值由用户从后台取）。
- ★ **未配置密钥 ⇒ 端点返回 404**（与 FeishuClient 为 nil 同款「明确拒绝，不静默降级」）。

### D4.2 事件处理
- ★ `contact.user.created_v3`（入职）⇒ upsert `s_user`（无角色，同 D3）。
- ★ `contact.user.deleted_v3`（离职）⇒ **停用账号**：**该用户所有活跃会话立即失效 ＋ 拒绝再登录 ＋ 写审计**。实现方式由你设计（如 `s_user.status` 或 deny 名单），要求：**离职员工不能再登录，且正在线的会话当场失效**（这是安全要求，不是体验要求）。
- ★ 其余事件类型：**忽略但记 debug 日志**（不报错）。

**验收**：单测（challenge 往返、签名错 ⇒ 403、离职事件 ⇒ 会话失效、created ⇒ upsert 幂等）。
**联调验收依赖用户**（飞书后台配事件订阅 + 回调地址 `http://office.hunanyichu.com:5502/feishu/event` + 权限 + 发版）⇒ 开发期以单测为准，**联调单独排**。

---

## D5 · 交付与判据

- ★ `bash scripts/check_all.sh` **必绿 11/11**（会报项可空）。
- ★ **前端内嵌铁律**：改 `web/src/**` 必须 `bash scripts/build.sh` 重建 dist。
- ★ **部署到测试服务器**（`bash scripts/deploy-test-server.sh --restart --smoke`）并做 D1/D2 的 curl 验收。
- ★ 回执写在 `COLLAB.md` 的 **`### N-018` 段内**，状态改 **`MIMO-DONE`**（结构化字段匹配，散文提及不算）。
- ★ 提交用**显式路径**（禁止 `git add -A`）；**只本地提交，不 push**。
- ★ ★ **如实声明未做项**（例如：真实飞书通讯录联调、事件订阅联调——依赖用户后台配置），**不要把它们藏起来**。

---

## §6 口径要求（硬约束）

1. ★★★ **系统调试与验收一律在测试服务器（`192.168.10.50`）**；本机只编译/纯单测/门禁。见 `docs/05`。
2. ★★ **`COLLAB.md` 只能在自己议题段内局部追加**；禁止整体重写/格式化写回（`check_collab_anchors.py` 判红）。
3. ★★★ **引号与展开纪律**：含命令示例的字符串一律单引号、禁反引号；`ssh 'bash -s' <<EOF` 内起后台进程必须 `</dev/null`；写完复扫。
4. ★★ **时间戳取机器时钟**（`date`），禁止估计顺推。
5. ★★ **对外报文（飞书 API 请求/回调响应）必须配形状断言用例**（Authorization、字段名、challenge 往返）—— dev/单测之外这条路径没人验过。
6. ★ **不新增权限点**（51 点冻结）：D3.2 复用现有 `sys.*` 管理域点并说明选择理由。
7. ★ **同步只 upsert 不删除**；离职停用＝「会话全失效＋拒再登录＋审计」。
8. ❌ 不改 `spec/*.json`、`spec/schema.sql`、`docs/*` 冻结口径（要改先开议题）；❌ 不跳过/改期望任何失败测试。
9. ★ **本机外层是 PowerShell**：跑 bash 命令用单条 `bash -lc "..."` 形式；★ 编辑文件前先 `Read`（本仓 `core.autocrlf=true`，字符串匹配易失败）。

## §8 易错点（本轮相关）

- ★ 飞书**通讯录权限范围** ≠ 应用**可用范围** ≠ **API 权限** —— 三层都要对，缺一层就「查不到人」，且**改完要发版**才生效。
- ★ `tenant_access_token` 与 `user_access_token` 的**过滤范围不同**：服务端同步必须用 **tenant**（应用身份）。
- ★ 敏感字段（手机号）有**独立字段权限**；一期**不需要手机号**就别申请，最小权限。
- ★ 事件回调的 **challenge** 与**验签**失败必须是 **4xx 且不同响应体**，不能混。
