# MIMO-NEXT-FIX-09 —— 清库重置脚本 ＋ 首个飞书登录自动 sysadmin（N-025）

> 交付方：**mimo code**　提出方：WorkBuddy　对应议题：**`N-025`**
> ★ 用户定案（2026-10-18）：功能实验从**干净库**开始；**账号表（m_user）与角色绑定也清**，用户将**重新同步**同事并分配角色；**保留角色定义与权限矩阵**（sysadmin 的 28 条授权已配好，新 sysadmin 登录后矩阵直接可用）。
> ★★ **配套新机制：首个飞书登录用户自动成为 sysadmin**（否则清库后无人有权限，用户无法开展配置）。

## D1 · 首个飞书登录自动 sysadmin（bootstrap-on-first-login）

- **触发条件（三条同时满足）**：
  1. 本次登录来源 ＝ **feishu_login**（**dev 桩登录不触发**——防止 `ou_dev_admin` 抢占）
  2. `s_user_role` 中**不存在** `sysadmin` 绑定
  3. 环境变量 `JX_BOOTSTRAP_ADMIN` **未配置**
- **行为**：`openSession` 成功（EnsureUser 之后）⇒ 自动绑定该用户 sysadmin ⇒ **写审计**（action=`bootstrap_sysadmin`，actor＝该用户）⇒ 登录响应/日志明示「已引导为系统管理员」。
- ★ **与现有 `JX_BOOTSTRAP_ADMIN` 的关系**：该变量**配置了 ⇒ 启动引导行为不变**（mimo TC 依赖）；条件 ③ 保证两机制互斥。★ **两者都未触发 ⇒ 系统处于「无 sysadmin」态**，等首个飞书登录。
- ★ 幂等：绑定前二次确认条件；重复登录不重复绑。
- ★ **测试（TC 至少 4 条）**：①无绑定＋未配置＋feishu_login ⇒ 自动绑定＋审计 ②dev_login 不触发 ③已有 sysadmin 绑定 ⇒ 不触发 ④`JX_BOOTSTRAP_ADMIN` 配置时启动引导不变、feishu_login 不重复触发。

## D2 · 重置脚本 `scripts/reset_db.sh`

- **安全设计**：★ **默认 dry-run**（仅列出将清空的表与行数）；**显式 `--force` 才执行**；执行**第一步＝mysqldump 全库备份**（文件名带时间戳，存 `~/jx-lab-trace/backups/`，**保留不删**）。
- **清空清单（TRUNCATE）**：
  - 业务：`b_arrival_notice, b_truck_lot, b_bag, b_label_print, b_obj_void, b_sample_group, b_sample, b_sample_retention, b_sample_lend, b_sample_destroy, b_inspection, b_inspection_result, b_inspection_file, b_production_batch, b_batch_operation, b_feed_record, b_fg_lot, b_fg_bag, b_rework, b_shipment, b_shipment_item, b_share_report, b_share_access`
  - 批次：`m_incoming_batch`
  - 主数据（★ 全为试验/联调数据，用户确认清）：`m_customer, m_composition, m_material_type, m_material, m_test_item, m_test_item_limit, m_vehicle, m_team, m_user`
  - 系统：`s_session, s_user_role, s_audit_log`
- **保留（绝不触碰）**：`s_permission_point, s_role, s_role_permission`
- **校验**：执行后断言——清空表行数＝0；三张保留表行数与执行前一致；输出重置报告（备份文件名＋各表行数）。
- ★ **输出提醒**：「重置后如重启服务，须先确认 `.env` 中 `JX_BOOTSTRAP_ADMIN` 已移除，否则启动引导将抢占首个 sysadmin」（运维动作由 WorkBuddy 执行）。

## D3 · 依赖排查

- ★ **排查测试夹具**：TC 是否依赖 `JX_BOOTSTRAP_ADMIN` 启动引导／预置角色绑定——受影响的夹具**自建绑定**（现有 m3-m8 夹具已批次化自建，核对即可）。
- ★ **排查 `openSession`→`EnsureUser`**：清空 `m_user` 后飞书登录自动重建本人账号——确认该路径在 `m_user` 空表时正常（应正常，既有逻辑）。

## D5 · 交付

- ★ 门禁 **11/11**；部署测试服务器（`.env` 红线）；**curl 实测**：TC 四条 ＋ 重置脚本 dry-run 演示（**不在生产数据上执行 --force**——真实重置由 WorkBuddy 在部署后按用户指令执行）。
- ★ 回执写 `COLLAB.md` **`### N-025` 段内**、状态 **`MIMO-DONE`**（**替换**原 OPEN 行）；显式路径提交；只本地提交不 push。

## §6 口径要求

1. ★★ **`s_role / s_role_permission / s_permission_point` 三表在任何代码路径不得被写**（除既有权限管理 API）——重置脚本与 bootstrap 均不触碰。
2. ★ **bootstrap 单实例简单实现**（事务内判定＋绑定）；不引入新依赖。
3. ★ 其余同 FIX-02 §6（服务器验收 / 引号纪律 / 时间戳 / COLLAB 局部追加 / 部署用已提交 commit）。
