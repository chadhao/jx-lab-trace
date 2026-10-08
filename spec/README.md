# spec/ —— 机读规格（唯一真相源）

> 本目录是 **`jx-lab-trace` 的机读契约**。当它与人读文档冲突时，**以本目录为准**。
> 所有规格均配**自校验脚本**——因为「看起来对、用起来才发现」的错，只能靠机器算一遍才现形。

## 文件

| 文件 | 内容 | 自校验 |
|---|---|---|
| `code-rules.json` | 追踪码规则：段定义 · 对象类型与层级 · 序号空间 · 校验算法参数 · 生成时机 · 解析步骤 · **5 条示例向量（含核算过的校验位）** · 人读行分组宽度 | `verify_code_rules.py` |
| `verify_code_rules.py` | 校验：段宽之和 · 校验位复算 · `full == payload+check` · 人读行同构 · **「三段序 = 三层嵌套」不变量** · 相邻换位可检出性 | — |
| `permission-points.json` | 权限点字典（**51 项**）· 角色（6 个）· 初始授权种子 · 约束 | `verify_permission_points.py` |
| `verify_permission_points.py` | 校验 8 组：code 唯一 · levels 合法 · seed 对齐 · **系统管理员无业务权限** · **无悬空点/无空闲角色** · **发起 ≠ 审批** · **防锁死** | — |
| `schema.sql` | 建表 DDL（**38 张**，MySQL 8.0） | 已在 MySQL 实测建表通过 |

## 怎么跑

```bash
python spec/verify_code_rules.py            # 退出码 0 = 通过
python spec/verify_permission_points.py     # 退出码 0 = 通过
```

★ **必须以退出码表达失败**（`sys.exit`），否则脚本会在管道里被静默吃掉。
★ 两个脚本均纳入 `scripts/check_all.sh` 的**必绿基线**。

## 新增规格的门槛

1. 必须是**机读**的（JSON/SQL/Python，不是散文）；
2. 必须配**自校验**（至少覆盖：结构自洽 + 已核算的示例向量）；
3. 必须被 `check_all.sh` 消费（**写了没人跑的规格 = 假配置**）。

## 与代码的关系

★ **共同契约，两侧各实现小引擎**：规格是唯一的语义来源，Go 侧与 Python 侧各自实现解析/校验，
**零引擎改动即两侧自动一致** —— 这是避免"两份真相"的核心手段。
