#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
check_uc_tc.py —— 「用例↔测试用例」覆盖门禁。

★ 为什么需要它：`docs/04-模块设计与用例.md` 里写了用例，但**没有任何机制保证有人验它**
  ⇒ 典型形态「**声明了却没人执行**」（同族：`immutable`、写了没人读的判据）。
  本门禁把「每条 UC 至少有一条 TC」变成**机检断言**。

判据：
  ① 每条 `UC-*` 至少被一条 `TC-*` 的「对应」列引用
  ② 每条 `TC-*` 引用的 `UC-*` 必须存在
  ③ `UC-*` / `TC-*` 编号不重复
  ④ 每个模块（`# M<n> ·`）至少有 1 条 UC 与 1 条 TC

退出码：0 = 通过；1 = 有失败项。
"""
import io
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
DOC = os.path.join(os.path.dirname(HERE), "docs", "04-模块设计与用例.md")

MODULE_RE = re.compile(r"^#\s+(M\d+)\s*[·\-]")
UC_RE = re.compile(r"\*\*(UC-[A-Z0-9]+-\d{2})\*\*")
TC_RE = re.compile(r"\*\*(TC-[A-Z0-9]+-\d{2})\*\*")
# 「对应」列里的引用：形如 UC-01 / UC-M0-01 / UC-01/03 / UC-01、UC-02
REF_RE = re.compile(r"UC-(?:[A-Z0-9]+-)?\d{2}")


def main():
    if not os.path.isfile(DOC):
        print("FAIL 找不到 %s" % DOC)
        return 1

    lines = io.open(DOC, encoding="utf-8").read().split("\n")

    cur_module = None
    module_of = {}          # 简称（如 UC-01）→ 全称（UC-M0-01）
    ucs = []                # 全称，按出现顺序
    tcs = []                # 全称
    tc_refs = {}            # TC 全称 → [引用到的 UC 全称]

    for raw in lines:
        m = MODULE_RE.match(raw)
        if m:
            cur_module = m.group(1)
            continue

        # UC 行：收集本行所有 UC 全称
        if "|" in raw and "UC-" in raw:
            pass
        for full in UC_RE.findall(raw):
            short = full.split("-", 1)[1]          # M0-01
            module_of[short] = full
            module_of[short.split("-")[-1]] = full  # 01 → UC-M0-01（模块内唯一）
            if full not in ucs:
                ucs.append(full)

        found_tcs = TC_RE.findall(raw)
        for full in found_tcs:
            if full not in tcs:
                tcs.append(full)
            # 只在 TC 行上解析「对应」列
            refs = []
            for r in REF_RE.findall(raw):
                r = r[3:]                            # 去掉 "UC-"
                if cur_module and not re.match(r"^[A-Z0-9]+-", r):
                    r = "%s-%s" % (cur_module, r)    # 01 → M0-01
                refs.append("UC-" + r)
            if refs:
                tc_refs.setdefault(full, [])
                for r in refs:
                    if r not in tc_refs[full]:
                        tc_refs[full].append(r)

    problems = []

    # ③ 编号不重复
    for name, seq in (("UC", ucs), ("TC", tcs)):
        dup = sorted({x for x in seq if seq.count(x) > 1})
        if dup:
            problems.append("[③] %s 编号重复：%s" % (name, ", ".join(dup)))

    uc_set = set(ucs)
    referenced = set()

    # ② TC 引用的 UC 必须存在
    for tc in tcs:
        refs = tc_refs.get(tc, [])
        if not refs:
            problems.append("[①] %s 没有「对应」列引用（每条 TC 必须指向一条 UC）" % tc)
            continue
        for r in refs:
            referenced.add(r)
            if r not in uc_set:
                problems.append("[②] %s 引用了不存在的 %s" % (tc, r))

    # ① 每条 UC 至少被一条 TC 引用
    orphan = [u for u in ucs if u not in referenced]
    if orphan:
        problems.append("[①] 以下 UC 没有任何 TC 验证：%s" % ", ".join(orphan))

    # ④ 每个模块至少 1 UC + 1 TC
    mods_uc, mods_tc = set(), set()
    for u in ucs:
        mods_uc.add(u.split("-")[1])
    for t in tcs:
        mods_tc.add(t.split("-")[1])
    for m in sorted(mods_uc | mods_tc):
        if m not in mods_uc:
            problems.append("[④] 模块 %s 没有任何 UC" % m)
        if m not in mods_tc:
            problems.append("[④] 模块 %s 没有任何 TC" % m)

    print("已扫：%s" % os.path.relpath(DOC, os.path.dirname(HERE)))
    print("统计：UC %d 条 · TC %d 条 · 被引用 UC %d 条" % (len(ucs), len(tcs), len(referenced)))
    if problems:
        print("FAIL 用例覆盖门禁未通过（%d 项）：" % len(problems))
        for p in problems:
            print("  · %s" % p)
        return 1
    print("OK 每条 UC 都有 TC 覆盖，且无悬空引用")
    return 0


if __name__ == "__main__":
    sys.exit(main())
