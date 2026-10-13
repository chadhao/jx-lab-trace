# MIMO-NEXT-FIX-05 —— 修「销毁记录列表」守卫挂错权限点（N-021 变更单）

> 交付方：**mimo code**　提出方：WorkBuddy　对应议题：**`N-021`**
> ★ 背景：用户（management＋sysadmin 双角色）与纯 sysadmin 账号实测，`GET /api/sample/destroy` 均 **403**，响应体 `point=sample.retain.destroy.init, need=READ`。

## 根因（已定案）

- ★ **`GET /api/sample/destroy`（销毁记录列表，含 `?pending=1`）的守卫挂了 `RequirePerm(sample.retain.destroy.init, access.LevelRead)`**。
- ★ **`sample.retain.destroy.init` 是"动作型"权限点**（级别体系＝INIT / APPROVE，供销毁发起/审批两个动作用），**不存在 READ 级别** ⇒ `Allows("READ")` 恒 false ⇒ **任何角色（含全授权的 sysadmin）都 403** —— 列表接口对所有人不可用。
- ★ 佐证：用户矩阵截图（sysadmin 列该点＝INIT）；服务器实测（双账号 403，响应体 point/need 如上）。

## D1 · 修复

- ★ **改挂业务语义正确的既有权限点**：推荐 **`sample.retain.in` ＋ `LevelRead`**（销毁记录属留样域的读；与「留样记录」列表同口径）—— ★ 若你判断有更贴切的**既有**点，可改并说明理由；★ **不新增权限点**（51 点冻结）。
- ★ **顺带核查同组路由**：`/api/sample` 组下其他 GET 列表（lend / samples 等）的挂点语义是否同类正确，发现同类错挂一并修（在回执列清单）。
- ★ **`destroy/init` 与 `destroy/approve` 两个动作 POST 的守卫不动**（它们挂动作型点是正确的）。

## D2 · 测试

1. **回归**：全授权账号（如 `ou_dev_admin` 或构造）⇒ `GET /api/sample/destroy` **200**
2. **无角色账号** ⇒ 401/403（deny by default 不变）
3. **未登录** ⇒ 401
4. ★ `destroy/init` / `destroy/approve` 两个动作 POST 的守卫行为**不变**（动作型点回归）
5. 既有测试全绿

## D5 · 交付

- ★ 门禁 **11/11**；部署测试服务器；**curl 实测**：全授权账号 `GET /api/sample/destroy?pending=1` ⇒ **200**（对照修复前 403）。
- ★ 回执写 `COLLAB.md` **`### N-021` 段内**、状态 **`MIMO-DONE`**；显式路径提交；**只本地提交不 push**；部署用**已提交 commit**（不用 -dirty 工作区）。
- ★ 如实声明未做项。

---

## §6 口径要求（承接 FIX-02/04，另加）

1. ★★★ **改挂点必须说明语义理由**（回执），且**不得新增权限点**。
2. ★ **动作型权限点（INIT/APPROVE 型）不用于 GET 列表守卫** —— 本缺陷的通用教训，回执里确认全仓没有同类错挂（用路由表逐一核对，**不要用源码 grep 拼接串的方式**——会漏，本次即漏）。
3. ★ 其余同 FIX-02 §6（服务器验收 / 引号纪律 / 时间戳 / COLLAB 局部追加 / 部署用已提交 commit）。
