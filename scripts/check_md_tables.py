#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Markdown 表格列数一致性门禁。

起因：03-TestCase.md 的变更记录表**表头 3 列、末三行 4 列** —— Markdown 渲染时
多出的单元格会被丢弃或错位，属"看起来没问题"的静默缺陷。本脚本把指定范围的
Markdown 表格按块检查「表头列数 == 分隔行列数 == 每行数据列数」。

★ 只检查"连续以 | 开头的行"构成的块，遇空行/非表格行即结算。
★ 单元格里的 `\\|`（转义竖线）必须还原后再计数，否则 `flock(LOCK_EX\\|LOCK_NB)` 会被误判成多一列。

★★ 扫描范围（v2，2026-10-01 扩围）：`SCAN_ROOTS` 里**显式声明**，且**逐根校验非空**。
★ 为什么不只用「一个目录」：v1 只扫 `docs/`，于是 **`COLLAB.md` / `MIMO-*.md` / `README.md`
  （都在仓库根）从未被检查过** —— ★ 而 `COLLAB.md` 是**双 Agent 唯一的协商渠道**、
  `MIMO-NEXT-BATCH-*.md` 是**任务包**（含 key 对照表等）；★ 它们坏掉是**静默**的。
★★ 更值得记的是**这个缺陷的形态**：v1 的修复（`3f76985`）已经堵住了「扫到 0 个文件」，
  并在报告里带上「已扫 N 个文件」让假绿无处藏身 —— ★ **但「扫了 21 个文件全通过」
  与「扫了该扫的 27 个文件全通过」在输出上依然无法区分**。★ 即：**「部分覆盖」
  冒充「全部覆盖」**，是同一族缺陷的**下一个形态**。
  ⇒ 故本版：① 范围**显式声明**；② 报告**逐根列出计数**（范围本身可见）；
    ③ 每个根**必须存在且非空**，否则非 0 退出（根消失 = 覆盖悄悄缩小 = 又是静默假绿）。
"""
import os
import re
import sys

ESC_PIPE = "\x00ESC_PIPE\x00"

# ★★ 扫描范围**显式声明**（根相对仓库、是否递归、该根最少应有的 .md 数）。
# ★ 为什么**每个根都要求非空**：某一个根被改名/移走 ⇒ 覆盖悄悄缩小 ⇒ 又变回静默假绿。
# ★ 排除目录：隐藏目录与第三方依赖（`node_modules` 等）不属交付物。
SKIP_DIRS = {".git", ".workbuddy", "node_modules", "dist", "_build", "_scratch", ".mimocode"}


def scan_roots() -> tuple:
    """→ ([(显示名, [文件路径...])], [(显示名, 原因)...])"""
    here = os.path.dirname(os.path.abspath(__file__))
    repo = os.path.dirname(here)
    # (相对仓库的路径, 是否递归, 最少文件数, 显示名)
    declared = (
        ("docs", True, 1, "docs/**/*.md"),
        (".", False, 1, "仓库根 *.md"),
        ("spec", True, 1, "spec/**/*.md"),
    )
    found_roots, problems = [], []
    for rel, recursive, min_files, label in declared:
        root = os.path.normpath(os.path.join(repo, rel))
        if not os.path.isdir(root):
            problems.append((label, f"目录不存在（找的是 {root}）"))
            continue
        files = []
        if recursive:
            for dirpath, dirnames, filenames in os.walk(root):
                dirnames[:] = [d for d in dirnames if not d.startswith(".") and d not in SKIP_DIRS]
                files += [os.path.join(dirpath, f) for f in sorted(filenames) if f.endswith(".md")]
        else:
            files = [os.path.join(root, f) for f in sorted(os.listdir(root))
                     if f.endswith(".md") and os.path.isfile(os.path.join(root, f))]
        if len(files) < min_files:
            problems.append((label, f"应至少有 {min_files} 个 .md，实际 {len(files)} 个"))
            continue
        found_roots.append((label, files))
    return found_roots, problems


def cells(line: str) -> int:
    s = line.strip()
    s = s.replace("\\|", ESC_PIPE)          # 转义竖线先藏起来
    if s.startswith("|"):
        s = s[1:]
    if s.endswith("|"):
        s = s[:-1]
    return len(s.split("|"))


def is_table_row(line: str) -> bool:
    return line.strip().startswith("|")


def is_sep_row(line: str) -> bool:
    s = line.strip()
    if not s.startswith("|"):
        return False
    s = s.replace("\\|", ESC_PIPE)
    body = s.strip("|")
    if not body.strip():
        return False
    # 允许 :---: / --- / :-- 等
    return all(re.fullmatch(r"\s*:?-{2,}:?\s*", c) for c in body.split("|"))


def check_file(path: str) -> list:
    problems = []
    with open(path, "r", encoding="utf-8", newline="") as f:
        lines = f.read().splitlines()

    i = 0
    while i < len(lines):
        if not is_table_row(lines[i]):
            i += 1
            continue
        start = i
        block = []
        while i < len(lines) and is_table_row(lines[i]):
            block.append((i + 1, lines[i]))
            i += 1

        if len(block) < 2:
            continue

        head_n = cells(block[0][1])
        # 块内第二个非分隔行若为分隔行，则以它为准（表头可能被误写）
        sep_n = None
        for ln, txt in block[1:3]:
            if is_sep_row(txt):
                sep_n = cells(txt)
                break

        if sep_n is None:
            problems.append((start + 1, "表格块缺少分隔行（|---|）", head_n, None))
            continue
        if sep_n != head_n:
            problems.append((start + 1, "表头列数 != 分隔行列数", head_n, sep_n))

        for ln, txt in block:
            if is_sep_row(txt):
                continue
            n = cells(txt)
            if n != head_n:
                problems.append((ln, "数据行列数与表头不一致", head_n, n))
    return problems


def main() -> int:
    roots, root_problems = scan_roots()
    if root_problems:
        print("FAIL 扫描范围不完整（★ 覆盖缩小 = 静默假绿，故非 0 退出）：")
        for label, why in root_problems:
            print(f"  · {label}：{why}")
        return 2

    scanned = 0
    total = 0
    per_root = []
    for label, files in roots:
        scanned_here = 0
        for path in files:
            scanned += 1
            scanned_here += 1
            rel = os.path.relpath(path).replace("\\", "/")
            for ln, msg, want, got in check_file(path):
                total += 1
                print(f"FAIL {rel}:{ln} {msg}（期望 {want}，实际 {got}）")
        per_root.append(f"{label}={scanned_here}")
        print(f"  · 已扫 {label}：{scanned_here} 个文件")

    scope = "＋".join(per_root)
    if scanned == 0:
        print("FAIL 扫描范围内没有 .md 文件；门禁未实际检查任何内容")
        return 2
    if total:
        print(f"\n共 {total} 处表格列数不一致（已扫 {scanned} 个文件：{scope}）")
        return 1
    print(f"OK 全部 Markdown 表格列数一致（已扫 {scanned} 个文件：{scope}）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
