# APP 业务 API · 批次 A+B 字段级契约（N-029）

> 状态：**已交付（MIMO-DONE）**　交付方：mimo code　提出方：APP 设计侧（`BACKEND-REQ-06.md` P0 路线图），经 WorkBuddy 转达
> 适用：APP 前端接入　**本文已定稿（V1.0）**　★ 字段契约以本文为准；Web 端契约不在本文范围内（互不影响）

## 0. 一句话

APP 端点是**薄层**：鉴权 + 入参校验 + 调 store + 形状包装，业务逻辑与 Web 端**同源**（同一 store 方法）；本文给出批次 A（首页聚合）与批次 B（收货域六动作）的**逐字段**请求/响应契约、错误码与拦截页字段说明。

**范围**：本批只含批次 A + 批次 B；**批次 C（生产/出货）与批次 D（追溯/报表）本批不做，属后续批次**。

## 1. 文档信息

| 项 | 值 |
|---|---|
| 版本 | V1.0 |
| 对应议题 | N-029 |
| 对应任务包 | `MIMO-NEXT-FIX-13.md` |
| 服务端实现 | `internal/httpapi/app.go`（端点）、`internal/store/app_read.go`（只读聚合） |
| 覆盖端点数 | 7（`GET /api/app/home` + 收货域 6 个动作） |
| 权限点 | 0 个新增（全部复用既有 51 点字典中的 `recv.*` 等现场点） |
| 最后更新 | 2026-10-21 |

## 2. 通用约定（全部 `/api/app/*` 端点）

| 约定 | 说明 |
|---|---|
| 前缀 | `/api/app/*`，与 Web 端 `/api/*` **并列**；不复用 Web 路由（字段形状互不耦合） |
| 鉴权 | Cookie `jx_sid` 会话（飞书登录 / Applink `exchange` / dev 免登桩同源）；未登录 ⇒ `401` |
| 权限 | 每端点 `RequirePerm(既有权限点, 级别)`；级别与 Web 端同点同级；**不新增权限点** |
| 序号 | 车序 / 袋序 / 打印留痕序号**全部由后端分配**，APP 不计算、不回传 |
| 时间戳 | `generated_at` 为 RFC3339（含时区）；`arrive_date` 等日期为 `YYYY-MM-DD`；`at` 类时间戳为 `YYYY-MM-DD HH:MM:SS` |
| 数值 | 重量单位＝吨，落库 3 位小数；净重＝毛重 − 皮重（**实测口径**，服务端计算） |
| 审计 | 所有写动作照常落 `s_audit_log`，`actor`＝会话人（open_id） |
| 错误结构 | 4xx 统一为结构化拦截四字段（见 §5）；`500` 仅 `{"error":"内部错误"}`（不外泄内部细节） |

### 2.1 结构化拦截四字段

```json
{"error":"<短标签>","reason":"<人话原因>","owner":"<责任角色/部门>","contact":"<联系途径>"}
```

| 字段 | 类型 | 含义 |
|---|---|---|
| `error` | string | 短标签：`未登录` / `无权限` / `无预报` / `无车辆档案`，或 store 原文（业务拒绝） |
| `reason` | string | 面向现场的一句话原因（为什么被拦） |
| `owner` | string | 责任角色/部门（谁来解决） |
| `contact` | string | 联系途径（怎么找到对方） |

### 2.2 owner / contact 映射表

| 拦截类 | HTTP | `error` | `owner` | `contact` |
|---|---|---|---|---|
| 未登录 | 401 | 未登录 | 本人 / 系统管理员（登录会话） | 在飞书内重新打开本应用登录；仍失败联系系统管理员 |
| 无权限 | 403 | 无权限 | 系统管理员（权限配置） | 飞书联系系统管理员，请其在 Web「后台 › 权限矩阵」按角色调整 |
| 无预报 | 404 | 无预报 | 收货文员（预报登记） | 飞书联系收货文员在 Web「收货 › 预报登记」补录预报后再操作 |
| 无车辆档案 | 404 | 无车辆档案 | 主数据维护（车辆档案） | 飞书联系主数据维护在 Web「主数据 › 车辆档案」补建档案 |
| 收货域业务拒绝 | 400/404/409 | store 原文 | 收货文员（收货与打码） | 飞书联系收货文员按页面提示处理 |

## 3. 批次 A · `GET /api/app/home`

**权限**：`RequireLogin`（跨域只读聚合，**没有单一对应的现场权限点** ⇒ 登录必达，可见性按域判 READ；不新增权限点）。
**查询参数**：无。

### 3.1 响应字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `me.open_id` | string | 登录人 open_id |
| `me.name` | string | 登录人姓名（无主数据行时为空串） |
| `me.roles` | string[] | 登录人角色 code 数组（业务角色 + 系统角色） |
| `domains` | string[] | 本次可见域数组（`recv` / `insp` / `prod` / `ship`），字典序 |
| `in_progress` | object[] | 进行中作业摘要（仅**可见**项；顺序见 3.3） |
| `in_progress[].key` | string | 聚合项键（见 3.3） |
| `in_progress[].label` | string | 中文标签 |
| `in_progress[].domain` | string | 所属域 |
| `in_progress[].count` | int | 进行中计数（`0` 也返回该键） |
| `in_progress[].latest` | object\|null | 最新一条摘要；计数为 0 时为 `null` |
| `in_progress[].latest.id` | int | 该单据主键 |
| `in_progress[].latest.code` | string | 单据号/车次码/批次号 |
| `in_progress[].latest.title` | string | 摘要文案（车牌 / 客户批次号 / 袋序 等） |
| `in_progress[].latest.status` | string | 单据状态 |
| `in_progress[].latest.at` | string | 时间 `YYYY-MM-DD HH:MM:SS` |
| `todos` | object[] | 今日待办计数（仅可见项） |
| `todos[].key` | string | 待办键（见 3.4） |
| `todos[].label` | string | 中文标签 |
| `todos[].domain` | string | 所属域 |
| `todos[].count` | int | 计数 |
| `recent` | object[] | 本人最近作业（最多 10 条，按 `id` 倒序） |
| `recent[].id` | int | 审计 id |
| `recent[].at` | string | 发生时间 `YYYY-MM-DD HH:MM:SS` |
| `recent[].entity` | string | 对象表名（如 `b_arrival_notice`） |
| `recent[].entity_id` | int | 对象 id |
| `recent[].action` | string | 动作（`create` / `arrive` / `bag_gen` …） |
| `recent[].field` | string | 变更字段（无则空串） |
| `recent[].new_value` | string | 新值摘要（无则空串） |
| `recent[].reason` | string | 填写原因（无则空串） |
| `recent[].actor_open_id` | string | 操作者 open_id（恒为登录人） |
| `recent[].actor` | string | 操作者姓名 |
| `recent_source` | string | 数据源说明（固定文案） |
| `generated_at` | string | 生成时刻 RFC3339 |

### 3.2 响应示例（节选）

```json
{
  "me": {"open_id": "ou_x", "name": "张三", "roles": ["receiver"]},
  "domains": ["insp", "recv", "ship"],
  "in_progress": [
    {"key": "notice_open", "label": "预报中车次", "domain": "recv", "count": 2,
     "latest": {"id": 12, "code": "N-20261021-003", "title": "湘F·12345 / 李师傅",
                "status": "预报", "at": "2026-10-21 09:12:00"}},
    {"key": "truck_pending", "label": "待检车次", "domain": "insp", "count": 0, "latest": null}
  ],
  "todos": [
    {"key": "await_receive", "label": "待收车数", "domain": "recv", "count": 1},
    {"key": "await_label", "label": "待打码数", "domain": "recv", "count": 2},
    {"key": "await_load", "label": "待装车数", "domain": "ship", "count": 0}
  ],
  "recent": [
    {"id": 88, "at": "2026-10-21 09:20:00", "entity": "b_arrival_notice", "entity_id": 12,
     "action": "create", "field": "", "new_value": "", "reason": "",
     "actor_open_id": "ou_x", "actor": "张三"}
  ],
  "recent_source": "s_audit_log（actor=登录人，按 id 倒序取 10 条）",
  "generated_at": "2026-10-21T09:20:31+08:00"
}
```

### 3.3 进行中聚合项（`key` → 口径 → 可见性门）

| `key` | 标签 | 域 | 计数口径 | 可见权限点（READ 级） |
|---|---|---|---|---|
| `notice_open` | 预报中车次 | recv | `b_arrival_notice.status='预报'` | `recv.notice.create` |
| `batch_open` | 回货中批次 | recv | `m_incoming_batch.status='回货中'` | `recv.notice.create` |
| `bag_in_stock` | 在库吨袋 | recv | `b_bag.status='在库'`（原料吨袋） | `recv.bag.gen` |
| `truck_pending` | 待检车次 | insp | `b_truck_lot.status='待检'` | `insp.task.view` |
| `prod_running` | 进行中生产批 | prod | `b_production_batch.status='进行中'` | `prod.batch.create` |
| `fg_bag_ready` | 在库/已装吨袋 | ship | `b_fg_bag.status='在库'`（含已归集进未出场单的袋） | `ship.load.scan` |
| `ship_open` | 未出厂出货单 | ship | `b_shipment.status='已出厂'` 且 `ship_at IS NULL` | `ship.load.scan` |

### 3.4 今日待办口径

| `key` | 标签 | 域 | 口径 | 可见权限点（READ 级） |
|---|---|---|---|---|
| `await_receive` | 待收车数 | recv | 今日登记且仍为「预报」的车次（系统无 ETA，以 `DATE(created_at)=今天` 为准） | `recv.notice.create` |
| `await_label` | 待打码数 | recv | 已到货（已有车次）但 `bag_count=0`，且状态非「已退货/已作废」 | `recv.bag.gen` |
| `await_load` | 待装车数 | ship | 在库成品吨袋中、未被任何未撤销出货单归集的袋数 | `ship.load.scan` |

> 口径与实现同步自 `internal/store/app_read.go`；**只读聚合**，不含业务判断。

## 4. 批次 B · 收货域六动作

### 4.1 `GET /api/app/recv/notices` · 预报列表

| 项 | 值 |
|---|---|
| 权限点 | `recv.notice.create`，级别 `READ`（与 Web `GET /api/recv/notices` 同点同级） |
| 委托 | `store.ListNotices`（与 Web 同一方法、同一 `mode` 语义） |

**查询参数**

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `mode` | string | 否 | 缺省＝全部；`open`＝预报中（含旧版）；`pending`＝批次预报（`batch_id` 非空且预报中，排除旧版） |

**响应字段**

| 字段 | 类型 | 说明 |
|---|---|---|
| `count` | int | 行数 |
| `notices` | object[] | 预报行（按 `id` 倒序，上限 500） |
| `notices[].notice_id` | int | 预报 id（后续动作入参用） |
| `notices[].notice_no` | string | 预报单号 |
| `notices[].batch_id` | int | 所属来料批次 id（`0`＝旧版预报） |
| `notices[].our_batch_no` | string | 我方批次号 |
| `notices[].customer_batch_no` | string | 客户批次号 |
| `notices[].customer_name` | string | 客户名 |
| `notices[].material_name` | string | 产品名 |
| `notices[].seq_no` | int | 批次内车序（后端分配） |
| `notices[].plate_no` | string | 车牌 |
| `notices[].driver` | string | 司机 |
| `notices[].phone` | string | 电话 |
| `notices[].status` | string | `预报` / `已到货` / `已取消` / `空号` |
| `notices[].arrive_date` | string | 链根日期 `YYYY-MM-DD`（未确认到货时为空串） |
| `notices[].legacy` | bool | 是否旧版预报（`batch_id` 为空） |

**示例**：`GET /api/app/recv/notices?mode=pending`

```json
{
  "count": 1,
  "notices": [
    {"notice_id": 12, "notice_no": "YC-00012", "batch_id": 3,
     "our_batch_no": "F0001...", "customer_batch_no": "C-88",
     "customer_name": "某乳业", "material_name": "脱脂奶粉",
     "seq_no": 3, "plate_no": "湘F·12345", "driver": "李师傅", "phone": "13800000000",
     "status": "预报", "arrive_date": "", "legacy": false}
  ]
}
```

---

### 4.2 `POST /api/app/recv/arrive-confirm` · 到货确认

| 项 | 值 |
|---|---|
| 权限点 | `recv.arrive.confirm`，级别 `ALL` |
| 委托 | `store.ConfirmArrival`（与 Web `POST /api/recv/arrivals` **同一调用链**：27 位车次码/二维码生成、链根 `arrive_date` 写入机制一字不改） |

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `notice_id` | int | **是** | 预报单 id（`<=0` 或缺失 ⇒ `400 缺少 notice_id`；APP 端不做无预报直达到货） |
| `remark` | string | 否 | 备注，透传给 store |

**响应字段**

| 字段 | 类型 | 说明 |
|---|---|---|
| `confirmed` | bool | 恒为 `true` |
| `truck_lot_id` | int | 车次 id（后续过磅/打码入参） |
| `truck_code` | string | **27 位车次码**（A 类） |
| `truck_human` | string | 车次码人读格式 |
| `notice_id` | int | 对应预报 id |
| `arrive_date` | string | 链根日期 `YYYY-MM-DD` |
| `seq_no` | int | 批次内车序 |
| `plate_no` | string | 车牌 |
| `status` | string | 车次状态（新到货＝`待检`） |

**示例**

```json
// 请求
{"notice_id": 12, "remark": "现场卸货完成"}
// 响应
{"confirmed": true, "truck_lot_id": 41, "truck_code": "A2...",
 "truck_human": "A2-XXXX-XXXX-...", "notice_id": 12, "arrive_date": "2026-10-21",
 "seq_no": 3, "plate_no": "湘F·12345", "status": "待检"}
```

**本端点特有拦截**

| 场景 | HTTP | `error` |
|---|---|---|
| 预报不存在 / 已取消 | 404 | `无预报` |
| 预报引用的 `vehicle_id` 在 `m_vehicle` 无对应行 | 404 | `无车辆档案` |
| 预报状态非「预报」（重复确认） | 409 | store 原文 |

> ★ `无车辆档案` 是 **APP 端只读前置校验**（仅 `vehicle_id>0` 时触发）；**Web 端行为不变**（本就不校验车辆档案）。

---

### 4.3 `POST /api/app/recv/return` · 退车

| 项 | 值 |
|---|---|
| 权限点 | `recv.return`，级别 `ALL` |
| 委托 | `store.ReturnTruck`（与 Web `POST /api/recv/trucks/:id/return` 同一方法；APP 侧多一步**只读**的 `notice_id → 车次` 定位） |

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `notice_id` | int | **是** | 预报单 id（据此定位车次） |
| `reason` | string | 否 | 退车原因；空则沿用 store 默认「退车登记，整车袋码作废」（与 Web 同源，不额外加必填规则） |

**响应字段**

| 字段 | 类型 | 说明 |
|---|---|---|
| `returned` | bool | 恒为 `true` |
| `truck_lot_id` | int | 车次 id |
| `truck_code` | string | 27 位车次码 |
| `notice_id` | int | 对应预报 id |
| `status` | string | 终态（`已退货`） |
| `bag_count` | int | 终态有效袋数（`0`） |

**特有拦截**

| 场景 | HTTP | `error` |
|---|---|---|
| 预报不存在 | 404 | `无预报` |
| 预报尚未到货（无车次） | 409 | store 原文（`尚无车次`） |
| 无现行质检「退货」判定 | 409 | store 原文（`未经质检退货判定`） |
| 已有投料袋 | 409 | store 原文（`已有投料`） |

---

### 4.4 `POST /api/app/recv/weigh` · 过磅

| 项 | 值 |
|---|---|
| 权限点 | `recv.weigh`，级别 `ALL` |
| 委托 | `store.WeighTruck`（与 Web `POST /api/recv/trucks/:id/weigh` 同一方法；**净重＝毛重 − 皮重**由 store 以毫吨整数算术计算，3 位小数落库） |

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `truck_lot_id` | int | **是** | 车次 id |
| `gross_weight` | number\|string | **是** | 毛重（吨，>0，≤3 位小数） |
| `tare_weight` | number\|string | 条件必填 | 皮重（吨，≥0，净重须>0）。**省略时取该车历史皮重**（见 4.4.1）；无历史 ⇒ `400` 要求人工录入 |

**响应字段**

| 字段 | 类型 | 说明 |
|---|---|---|
| `weighed` | bool | 恒为 `true` |
| `truck_lot_id` | int | 车次 id |
| `truck_code` | string | 27 位车次码 |
| `gross_weight` | number | 毛重（吨） |
| `tare_weight` | number | 皮重（吨，实际入库值） |
| `net_weight` | number | 净重（吨）＝ 毛 − 皮 |
| `tare_source` | string | `input`＝入参给定；`history`＝取自历史皮重 |
| `tare_history` | object\|null | `tare_source=history` 时给出处；否则 `null` |
| `tare_history.truck_id` | int | 历史车次 id |
| `tare_history.tare_weight` | number | 历史皮重（吨） |
| `tare_history.arrive_date` | string | 历史到货日 `YYYY-MM-DD` |
| `status` | string | 车次状态 |

#### 4.4.1 皮重历史口径（★ 如实声明）

- 系统**原无**「历史皮重查询」机制；本批新增 `store.LastTareFor`（**只读查询**）：按 ① 同 `vehicle_id` ② 同车牌（经预报单）匹配**最近一次有皮重的车次**，排除当前车次自身。
- 取到历史 ⇒ 自动填充并以 `tare_source="history"` 明示；**取不到 ⇒ 不猜值**，返回 `400` 要求人工录入。
- 净重仍完全由 `store.WeighTruck` 计算（毛 − 皮，实测口径不变），历史值只是皮重入参来源。

**示例**

```json
// 请求（省略皮重 ⇒ 取历史）
{"truck_lot_id": 42, "gross_weight": 12}
// 响应
{"weighed": true, "truck_lot_id": 42, "truck_code": "A2...",
 "gross_weight": 12, "tare_weight": 3.25, "net_weight": 8.75,
 "tare_source": "history",
 "tare_history": {"truck_id": 41, "tare_weight": 3.25, "arrive_date": "2026-10-21"},
 "status": "待检"}
```

---

### 4.5 `POST /api/app/recv/bag-gen` · 建袋打码

| 项 | 值 |
|---|---|
| 权限点 | `recv.bag.gen`，级别 `ALL` |
| 委托 | `store.GenerateBags`（与 Web `POST /api/recv/trucks/:id/bags` 同一方法：袋序自动分配、摊算重量＝净重÷袋数 half-up、吨袋码生成全部在 store 内完成） |

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `truck_lot_id` | int | **是** | 车次 id |
| `count` | int | **是** | 袋数 `1..999`（序号由后端分配，APP 不传起始序号） |

**响应字段**

| 字段 | 类型 | 说明 |
|---|---|---|
| `generated` | int | 生成袋数 |
| `truck_lot_id` | int | 车次 id |
| `bags` | object[] | 袋行（按 `bag_seq` 升序；本车全量） |
| `bags[].bag_seq` | int | 袋序（后端分配，从 1 起） |
| `bags[].code` | string | 27 位吨袋码（B 类） |
| `bags[].human` | string | 吨袋码人读格式 |
| `bags[].weight_allocated` | number\|null | 摊算重量（吨）＝车净重 ÷ 袋数 |
| `bags[].weight_is_allocated` | int | 恒为 `1`（标记重量是摊算值，非实测） |
| `bags[].status` | string | `在库` |

**特有拦截**：未过磅 ⇒ `409 尚未过磅`；已有袋 ⇒ `409 已生成袋码不能重复生成`；已退货 ⇒ `409`；`count` 非法 ⇒ `400`。

---

### 4.6 `POST /api/app/label/reprint` · 标签补打

| 项 | 值 |
|---|---|
| 权限点 | `recv.label.reprint`，级别 `ALL` |
| 委托 | `store.PrintLabels`（`Reprint=true`，与 Web `POST /api/recv/labels/reprint` 同一方法；补打必填原因由 **store 校验**） |

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `bag_code` | string | **是** | 袋号（27 位吨袋码）；缺失 ⇒ `400 缺少 bag_code` |
| `reason` | string | **是** | 补打原因；空 ⇒ store 拒绝 `400 补打必须填写原因`（留痕防「一物两码」） |

**响应字段**

| 字段 | 类型 | 说明 |
|---|---|---|
| `count` | int | 本次打印记录数 |
| `labels` | object[] | 打印记录 |
| `labels[].label_id` | int | 打印记录 id |
| `labels[].code` | string | 被打印的码 |
| `labels[].human` | string | 人读格式 |
| `labels[].kind` | string | 对象类型（`原料吨袋` / `车次` 等） |
| `labels[].is_reprint` | int | `1`＝补打 |
| `labels[].reason` | string | 补打原因 |
| `labels[].printed_by` | string | 打印人 open_id |
| `page_url` | string | 标签版式页地址（服务端生成页面后调起打印） |

> `page_url` 指向 Web 版式页 `/api/recv/labels/page?ids=…`，打开需要 `recv.label.print` 的 `READ` 级；若 APP 只持 `recv.label.reprint`，请用 `labels[].code`/`human` 自行渲染。

## 5. 错误码与拦截页字段说明

| HTTP | 触发 | 响应体 |
|---|---|---|
| `200` | 成功 | 见各端点响应表 |
| `400` | 入参不合法 / 缺必填 / 补打缺原因 / 无历史皮重 | 四字段结构 |
| `401` | 未登录或会话失效 | 四字段结构（`error=未登录`） |
| `403` | 权限点或级别不足 | 四字段结构（`error=无权限`） |
| `404` | 无预报 / 无车辆档案 / 对象不存在 | 四字段结构 |
| `409` | 业务状态冲突（重复确认、未到货、无退货判定、重复打码 …） | 四字段结构 |
| `500` | 服务端内部错误 | `{"error":"内部错误"}`（**无** reason/owner/contact） |

**拦截页（APP 侧）建议呈现**：

1. 首行显示 `error`（短标签）作为标题；
2. 正文显示 `reason`（人话原因，禁止原样抛栈/英文报错）；
3. 底部固定展示 `owner`（谁负责）与 `contact`（怎么联系）；
4. `401` 时提供「重新登录」按钮；`403` 时不提供重试按钮（重试不会成功）；
5. `404 无预报` 提供「联系收货文员补录」入口；`404 无车辆档案` 提示找主数据维护建档；
6. `409` 提供「刷新」并按流程顺序（预报 → 到货 → 过磅 → 打码 → 检测）引导。

**Web 端错误结构不变**：`/api/*` 仍为 `{"error":…}` 或 `{"error","point","need","hint"}`；四字段结构只在 `/api/app/*` 出现。

## 6. 权限矩阵速查（本批 7 端点）

| 端点 | 权限点 | 级别 | receiver | qc | production | sales | sysadmin |
|---|---|---|---|---|---|---|---|
| `GET /api/app/home` | （登录） | — | 200 | 200 | 200 | 200 | 200（空聚合） |
| `GET /api/app/recv/notices` | `recv.notice.create` | READ | 200 | 200 | 403 | 200 | 403 |
| `POST /api/app/recv/arrive-confirm` | `recv.arrive.confirm` | ALL | 200 | 403 | 403 | 403 | 403 |
| `POST /api/app/recv/return` | `recv.return` | ALL | 200 | 403 | 403 | 403 | 403 |
| `POST /api/app/recv/weigh` | `recv.weigh` | ALL | 200 | 403 | 403 | 403 | 403 |
| `POST /api/app/recv/bag-gen` | `recv.bag.gen` | ALL | 200 | 403 | 403 | 403 | 403 |
| `POST /api/app/label/reprint` | `recv.label.reprint` | ALL | 200 | 403 | 403 | 403 | 403 |

> 表中 `200` 指**通过权限守卫**（业务上仍可能因数据状态返回 400/404/409）；`403` 为权限拦截。
> ★ 本表按 `spec/permission-points.json` 的**种子授权**推导；权限矩阵是**后台可配**的
>（`s_role_permission`），实际格子值以库内实况为准（APP 一律按 `401`/`403` 结构化拦截处理）。
> 种子本身 51 点 × 6 角色冻结，**不新增权限点**。

## 7. 本批未覆盖（如实声明）

| 项 | 说明 |
|---|---|
| 批次 C（生产 / 出货域端点） | 本批不做，属后续批次 |
| 批次 D（追溯 / 报表域端点） | 本批不做，属后续批次 |
| 无预报直达到货（APP） | APP 到货确认只接受 `notice_id`；该能力仅在 Web 端存在 |
| 历史皮重自动取值 | 新增只读查询实现（见 4.4.1）；系统原本无此机制，无历史时必须人工录入 |
| 车辆档案校验 | 仅 APP 端到货确认做只读前置拦截；Web 端行为不变 |

## 变更记录

| 版本 | 日期 | 变更 |
|---|---|---|
| V1.0 | 2026-10-21 | 首版：批次 A（home）+ 批次 B（收货六动作）字段级契约、错误与拦截、权限矩阵 |
