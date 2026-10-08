#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
check_collab_anchors.py —— `COLLAB.md`「我方关键内容」存在性门禁。

★ 为什么需要它（2026-10-09 实测事故）：
  mimo 把 `COLLAB.md` **整体重写**（它做了 markdown 表格格式化），而重写的底本是它**早先读到的旧版本**
  ⇒ 把 WorkBuddy 在它读取**之后**写入的内容**全部冲掉**（净删 97 行：铁律 8、约束变更通知、§1 推送状态…）。
  ★ 事后靠人眼发现，已经晚了几十分钟。
  ⇒ **把「我方内容不可被删」变成机检断言**：任何人（人或 Agent）跑 `check_all.sh` 都会立刻看见红。

★ 这条判据与铁律 3（不改写对方已写的内容）的关系：
  铁律 3 是**约定**，本门禁是**执行体** —— 约定没有执行体，等于没有约定。

退出码：0 = 锚点齐全；1 = 有缺失（**阻断提交**）。
"""
import io
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
COLLAB = os.path.join(ROOT, "COLLAB.md")

# ★ 锚点必须是**稳定**的：结构标题、铁律编号、以及不会因日常改写而变化的短语。
#   ⚠ 不要往这里塞"某次记录的具体句子" —— 那会让本门禁随着正常追加而误红。
ANCHORS = [
    # 结构（八节 + 两附录，缺一节即视为结构被破坏）
    "## 0. 铁律",
    "## 1. 当前状态",
    "## 2. 责任域划分",
    "## 3. 文档与规格索引",
    "## 4. 待议",
    "## 5. 已决议",
    "## 6. 已上交用户",
    "## 附录 A",
    "## 附录 B",
    # 附加铁律（WorkBuddy 所立，被删即视为内容回退）
    "附加铁律 6",
    "附加铁律 7",
    "附加铁律 8",
    "报障前三问",
    # §1 的既有行（★ 用**裸子串**，不要带 `**`／`★` 等装饰 ——
    #   实测首版写成 `**推送状态**` 就匹配不到，因为真实行是 `| **★ 推送状态** | … |`。
    #   ⇒ ★ 教训：**锚点必须先在真实文件上验证过**，否则判据自己就是假红/假绿源。）
    "推送状态",
    "门禁状态",
]


def main():
    # ★ 支持 --file 以便**探针自证**（用合成样本打「假阳性 / 真阳性」两条分支，
    #   而不必去动真台账）。默认仍是仓库根的 COLLAB.md。
    path = COLLAB
    if len(sys.argv) > 2 and sys.argv[1] == "--file":
        path = sys.argv[2]

    if not os.path.isfile(path):
        print("FAIL 找不到 %s" % path)
        return 1

    text = io.open(path, encoding="utf-8").read()
    lines = text.split("\n")

    print("已扫：%s（%d 行 · %d 字节）" % (os.path.basename(path), len(lines), len(text.encode("utf-8"))))

    missing = [a for a in ANCHORS if a not in text]

    # 附加判据：§0 铁律必须 5 条编号齐全（防"表格被重排时丢行"）
    for n in range(1, 6):
        if not re.search(r"\*\*%d\*\*" % n, text):
            missing.append("铁律编号 **%d**" % n)

    if not missing:
        print("OK 我方 %d 个锚点齐全" % len(ANCHORS))
        return 0

    print("")
    print("FAIL **Missing %d anchor(s)** —— 判据未通过（阻断提交）：" % len(missing))
    for m in missing:
        print("  · %s" % m)
    print("")
    print("★ 最可能的原因：**COLLAB.md 被整体重写/格式化，且底本是旧版本** ⇒")
    print("  WorkBuddy 在对方读取之后追加的内容被冲掉了。")
    print("★ 处置（**不要用 `git checkout --` 硬还原**，那会连对方已写的回执一起丢）：")
    print("  1. 先 `cp COLLAB.md /tmp/COLLAB.对方版.md` 留档；")
    print("  2. 取 HEAD 版逐条比对，**把对方真正新增的回执段合并进来**；")
    print("  3. 还原后核对 `sha256sum`；")
    print("  4. 在台账内登记事故（谁、何时、丢了什么、怎么防）。")
    return 1


if __name__ == "__main__":
    sys.exit(main())
