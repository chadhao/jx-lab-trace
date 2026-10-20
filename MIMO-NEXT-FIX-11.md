# MIMO-NEXT-FIX-11 —— APP 后端需求 02/03：Applink 交换端点 ＋ B-req 四项增强（N-027）

> 交付方：**mimo code**　提出方：APP 设计侧（`jx-lab-trace-app/docs/BACKEND-REQ-02.md`、`BACKEND-REQ-03.md`），经 WorkBuddy 转达　对应议题：**`N-027`**
> ★ 优先级：**REQ-03 高**（APP Applink 登录升级的依赖项）；REQ-02 四项为可选增强（B2/B3 中、B1/B4 低），用户已授权一次做完。

---

## D1 · 【REQ-03·高】新增 `POST /api/auth/feishu/exchange`（Applink 授权码直换会话）

### 契约（APP 侧文档为准）
- 请求：`Content-Type: application/json`，`{"code":"<飞书授权码>","state":"<APP 原样回传>"}`
- 成功：**`Set-Cookie: jx_sid=<sid>; HttpOnly; SameSite=Lax; Max-Age=43200`** ＋ `{"ok":true}`（APP 的 CookieJar 持有，后续自动带会话）
- 失败：401（code 无效/过期）／403（角色未映射，**带 `userMessage:"您的账号尚未分配角色，请联系综合运营部"`**）／502（飞书上游错误，详情只进服务端日志）

### 实现（复用现有链路，不另起炉灶）
1. ★ **复用 `s.Feishu.Exchange(ctx, code, redirectURI)`**（与现有 callback 同一调用）做 code→open_id。
2. ★ **复用 `openSession` 全链**：deny-by-default 角色映射、**首登 bootstrap（`how="feishu_login"` 同样触发——清库后 APP 首登也要能拿 sysadmin）**、审计、TTL 12h。
3. ★ **兼容性**：`GET /feishu/callback` 与 `JX_DEV_MODE` 行为一字不改。
4. ★ **state**：APP 生成并内存保存（防 CSRF 在 APP 侧完成）——后端**不接受/不校验**其内容，作为不透明字符串忽略（响应不回显）。
5. ★★ **redirect_uri 一致性风险（必须实测）**：飞书 code 换身份接口可能校验 redirect_uri 与 authorize 时一致；APP 的 applink authorize 用的是它的 deep-link。**用假 code 实测报错形态**；若强校验 ⇒ exchange 入参增加可选 `redirect_uri`（APP 回传其 authorize 所用值）——回执记录实测结论与所选项。
6. ★ **PKCE（P1 建议）**：**先核飞书 `code 换身份` 接口是否支持 `code_verifier` 校验**——支持则实现（exchange 加 `code_verifier`）；不支持则**如实声明**「待飞书能力，APP 侧 P1 暂缓」，不阻塞上线。
7. ★ 限速：与现有 auth 端点同策略（若现有无专门限速，回执声明现状即可，不新造）。

## D2 · 【B-req-2·中】建出货单携带车辆

- ★ **发现**：`b_shipment` **已有** `vehicle_id/plate_no/driver` 列——缺的只是**建单入参**。`handleCreateShipment`（ship.go:62）入参加可选 `plate_no/driver/vehicle_id`，落库到单头。
- ★ **从车次上下文带出**（可选增强）：建单时若扫的成品袋可回溯车次，**自动预填** plate_no/driver（APP 端仍可改）——实现代价与谱系查询相同则做，否则仅做入参透传（回执声明）。
- ★ 出场 `DepartInput` 的 plate_no/driver 改为**预填只读**（后端无需改——APP 侧改；后端仅保证单头字段就位）。

## D3 · 【B-req-1·低】出货单净重聚合

- ★ `b_shipment_item`（shipment_id＋fg_bag_id）**JOIN `b_bag`**（weight_allocated 摊算值）：
  - items 明细每行暴露 `weight_allocated`
  - 单头暴露 **`net_weight_total`**（Σ 有效袋摊算重，作废袋不计——口径回执写明）
- ★ 挂在 `handleGetShipment`（ship.go:107）响应；列表接口若轻量可一并加汇总。

## D4 · 【B-req-3·中】追溯入口扩展（C/E 码）

- ★ codec 六类码解析已齐（A 车次/B 吨袋/C 生产批/D 成品批/E 成品袋/F 来料批次）——**瓶颈在 handler 的准入类型检查**。
- ① `GET /trace/batch/:batchId`：**支持传 C 生产批码**——先 `codec.ScanCode` 解析：数字 id 直用；C 码 ⇒ 按 code 查 `b_production_batch` 得 id；其它类型码 ⇒ 400 明确报错。
- ② `backward` 支持 **E 码**（成品袋码）入口：按现有谱系语义从 E 袋回溯（E→D 成品批→C 生产批→…）；`forward` 保持 B 码（如需求）。
- ★ 每个入口的**准入类型与报错文案**（「此入口请扫 X 码」）在回执列清单。

## D5 · 【B-req-4·低】生产批作废接口

- ★ `b_production_batch.status` **已含「已作废」**——缺的是接口。新增 **`PATCH /api/prod/batches/:id/void`**（或按现有风格）：
  - **限制**：仅「建批后未投料」——判定＝**该生产批下无任何投料记录**（`b_feed_record` 关联口径由 mimo 按 schema 核实并回执写明）；已投料 ⇒ 409 明确报错
  - 已完成/已作废 ⇒ 409（幂等语义回执说明）
  - 权限：**挂现有 `prod.*` 点**（不新增），选择理由写回执
  - 作废后：谱系/报告对该批的可见性处理（标注「已作废」即可，不隐藏历史）——回执说明
- ★ 审计照常（appendAudit）。

## D6 · 测试（每项都要 TC）

1. REQ-03：exchange 成功（Set-Cookie 断言＋`{"ok":true}`）／401／403 带 userMessage／502（mock 上游错）／**bootstrap 触发**（清库态首个 exchange 登录者获 sysadmin——对齐 N-025 机制）／dev 模式不受影响／现有 callback 回归
2. B-req-2：建单带车辆 ⇒ 单头落库并可查
3. B-req-1：明细 weight_allocated ＋ 头 net_weight_total（含作废袋不计的口径用例）
4. B-req-3：C 码解析进 /trace/batch；E 码 backward 谱系；错误类型码 400 文案
5. B-req-4：未投料作废成功／已投料 409／已完成 409／权限点回归
6. 形状断言逐字段；既有测试全绿

## D7 · 交付

- ★ 门禁 **11/11**；部署测试服务器（**部署前确认 `.env` 完好、部署后 healthz＝本次提交无 dirty**——N-020 红线）；**curl 实测**：exchange 全错误分支 ＋ B-req 各一正例。
- ★ 回执写 `COLLAB.md` **`### N-027` 段内**、状态 **`MIMO-DONE`**（**替换**）；显式路径提交；只本地提交不 push；如实声明未做项（尤其 PKCE 的飞书支持核实结论）。

## §6 口径要求

1. ★ **不新增权限点**：新端点挂现有 `prod.*`（作废）等；**exchange 是登录端点免权限**（同 callback）。
2. ★★ **callback/dev 模式行为一字不改**（兼容性红线）。
3. ★★ **`.env` 红线**（N-020）；部署用已提交 commit。
4. ★ 其余同 FIX-02 §6（服务器验收 / 引号纪律 / 时间戳 / COLLAB 局部追加 / 形状断言逐字段）。
