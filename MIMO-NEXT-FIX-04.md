# MIMO-NEXT-FIX-04 —— 修复 URL 校验（challenge）被误拒（N-019 变更单）

> 交付方：**mimo code**　提出方：WorkBuddy　对应议题：**`N-019`**
> ★ 背景：`N-018` 交付的事件回调端点对**所有**请求强制验签。实测（2026-10-12）：用户在飞书后台保存回调地址 ⇒ 飞书发 challenge ⇒ 端点 **403「签名验证失败」** ⇒ 后台报「**Challenge code 没有返回**」。

## 根因（已定案，证据链完整）

- ★ **一级来源（Lark 官方文档原文）**："Security verification when your local server receives events pushed by the Open Platform (**excluding request URL verification**)" ⇒ **URL 校验请求不参与签名校验**。
- ★ **我方判定性实验**：自构正确签名（`sha256(ts+nonce+key+body)` hex）⇒ challenge **200 正确应答**；错误签名 ⇒ 403。⇒ **验签算法本身正确**。
- ★ **用户「加密策略」页截图**：Encrypt Key = `P2Oio…`、Verification Token = `8Xdr…` ⇒ **与服务器配置一致，未拿反**（排除 A）。
- ⇒ **唯一自洽根因：对 `url_verification` 误施签名校验**。

---

## D1 · 修复

**当前顺序**：read raw → **验签** → decrypt → parse → 处理。
**改为**：read raw → **decrypt**（若 `{"encrypt":…}`）→ parse →
- **若 `type=url_verification`（或解密后含 `challenge`）**：⇒ **豁免签名校验**；校验 body 内 `token` ＝ 配置的 `JX_FEISHU_VERIFICATION_TOKEN`（**配置了才校验**；校验失败 ⇒ 403，响应体与签名失败**不同**）⇒ **200 回显 `challenge`**
- **否则（真实事件）**：⇒ **按现状强制验签**（用 **raw body**，事件路径不受影响）⇒ 处理事件

**要点**：
1. ★ **事件路径的验签一丝不改**（回归必须证明）。
2. ★ **兼容明文与加密两种 challenge 形态**（用户后台已配置 Encrypt Key ⇒ 实际是加密形态；明文兼容保留给单测）。
3. ★ **毫秒级响应**——官方要求 challenge **1 秒内**返回（比事件处理的 3 秒更严）。
4. ★ token 校验失败、签名失败的**响应体必须可区分**（§8 既有要求）。

## D2 · 测试（全部要有，缺一不可）

1. `url_verification`（**明文**、无签名头）⇒ 200 回显 challenge
2. `url_verification`（**加密**：用 Encrypt Key 按官方算法构造真实密文）⇒ 200 回显
3. `url_verification` **token 错误** ⇒ 4xx（响应体 ≠ 签名失败）
4. **回归**：真实事件（如 created_v3）**签名错 ⇒ 403**（不被豁免波及）；**签名对 ⇒ 正常处理**
5. 既有 `N-018` 测试全绿

## D5 · 交付

- ★ 门禁 **11/11**；部署到测试服务器；**curl 实测 challenge 往返**（明文构造即可，加密路径由单测覆盖）。
- ★ 回执写 `COLLAB.md` **`### N-019` 段内**、状态 **`MIMO-DONE`**；显式路径提交；**只本地提交不 push**。
- ★ 如实声明未做项（真实飞书 challenge 联调——依赖用户重新点保存）。

---

## §6 口径要求（承接 FIX-02 全部硬约束，另加）

1. ★★★ **challenge 必须 1 秒内返回**（官方口径，比事件的 3 秒更严）——处理路径禁止任何慢调用。
2. ★ **签名校验算法/头名一字不改**（已对照官方六语言示例核过：`sha256(timestamp+nonce+encrypt_key+rawBody)` hex）。
3. ★ 验签**使用 raw body**（加密原文），不得解密后再算。
4. ★★ 本机外层 shell 非 bash；编辑前先 `Read`；其余同 FIX-02 §6。
