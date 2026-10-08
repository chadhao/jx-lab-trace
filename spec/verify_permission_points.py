#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
权限点机读规格自校验（无第三方依赖）。

用途：校验 spec/permission-points.json 是否自洽，重点是那些「不报错但会让权限失效」的形态：
  ① 权限点 code 唯一；levels 合法且必含 NONE
  ② seed 的键集合 == 权限点集合（不重不漏）
  ③ seed 每条覆盖全部角色；取值必须在该点声明的 levels 之内
  ④ 系统管理员在【业务点】上一律 NONE（最小权限）
  ⑤ 每个业务点至少有一个角色不是 NONE（防「配了没人能用」）
  ⑥ 每个角色至少在一个点上不是 NONE（防「建了角色但什么都没给」）
  ⑦ 「发起 / 审批」成对：每个 .init 点必须有 .approve 兄弟，且【发起者 ≠ 审批者】
  ⑧ 防锁死：系统管理员的 5 个管理域点必须全为 ALL（不可被剥夺）

★ 为什么必须自校验：权限是「数据驱动」的，配错了不会报错 —— 只会静默地「没人能做这件事」
  或「谁都能做这件事」。这两种都属于最危险的那类缺陷（错误表现为正确）。

退出码：0 = 全部通过；1 = 有断言失败。
"""
import io
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SPEC = os.path.join(HERE, "permission-points.json")

failures = []


def check(cond, msg):
    if cond:
        print("  ok   %s" % msg)
    else:
        print("  FAIL %s" % msg)
        failures.append(msg)


def main():
    with io.open(SPEC, encoding="utf-8") as f:
        spec = json.load(f)

    levels = set(spec["levels"].keys())
    roles = [r["code"] for r in spec["roles"]]
    points = spec["permission_points"]
    seed = spec["seed"]
    biz_roles = [r["code"] for r in spec["roles"] if r["kind"] == "business"]
    sys_roles = [r["code"] for r in spec["roles"] if r["kind"] == "system"]

    print("[1] 权限点定义")
    codes = [p["code"] for p in points]
    check(len(codes) == len(set(codes)), "权限点 code 唯一（共 %d 项）" % len(codes))
    bad_lv = [p["code"] for p in points if not set(p["levels"]) <= levels]
    check(not bad_lv, "所有 levels 取值合法（越界：%s）" % (bad_lv or "无"))
    no_none = [p["code"] for p in points if "NONE" not in p["levels"]]
    check(not no_none, "每个权限点的 levels 都含 NONE（缺：%s）" % (no_none or "无"))
    bad_code = [c for c in codes if not c.replace(".", "").replace("_", "").isalnum()]
    check(not bad_code, "code 命名合法（异常：%s）" % (bad_code or "无"))

    print("[2] seed 与权限点对齐")
    check(set(seed.keys()) == set(codes),
          "seed 键集合 == 权限点集合（多出 %s / 缺失 %s）"
          % (sorted(set(seed) - set(codes)) or "无", sorted(set(codes) - set(seed)) or "无"))
    missing_role = []
    bad_level = []
    for c, m in seed.items():
        if set(m.keys()) != set(roles):
            missing_role.append(c)
        decl = set(next(p["levels"] for p in points if p["code"] == c))
        for r, lv in m.items():
            if lv not in decl:
                bad_level.append("%s[%s]=%s" % (c, r, lv))
    check(not missing_role, "seed 每行覆盖全部角色（缺：%s）" % (missing_role or "无"))
    check(not bad_level, "seed 取值都在该点声明的 levels 内（越界：%s）" % (bad_level or "无"))

    print("[3] 最小权限：系统管理员无业务权限")
    leaked = []
    for p in points:
        if p["module"] == "系统管理":
            continue
        for sr in sys_roles:
            if seed[p["code"]][sr] != "NONE":
                leaked.append("%s[%s]=%s" % (p["code"], sr, seed[p["code"]][sr]))
    check(not leaked, "系统管理员在全部业务点上为 NONE（泄漏：%s）" % (leaked or "无"))

    print("[4] 无悬空：权限点有人能用 / 角色有事可做")
    dead_point = [p["code"] for p in points
                  if all(seed[p["code"]][r] == "NONE" for r in roles)]
    check(not dead_point, "每个权限点至少有一个角色非 NONE（悬空：%s）" % (dead_point or "无"))
    # 业务角色必须至少在业务点上有一项非 NONE
    idle_role = []
    for r in biz_roles:
        holds = [p["code"] for p in points
                 if p["module"] != "系统管理" and seed[p["code"]][r] != "NONE"]
        if not holds:
            idle_role.append(r)
    check(not idle_role, "每个业务角色都有非 NONE 的业务权限（空闲：%s）" % (idle_role or "无"))

    print("[5] 发起 / 审批 成对，且发起者 ≠ 审批者")
    pairs_ok = True
    for c in codes:
        if not c.endswith(".init"):
            continue
        ap = c[:-len(".init")] + ".approve"
        if ap not in codes:
            check(False, "%s 缺少配对的 %s" % (c, ap))
            pairs_ok = False
            continue
        initers = [r for r in roles if seed[c][r] == "INIT"]
        approvers = [r for r in roles if seed[ap][r] == "APPROVE"]
        check(bool(initers) and bool(approvers),
              "%s 有发起者(%s)与审批者(%s)" % (c, initers, approvers))
        check(not (set(initers) & set(approvers)),
              "%s 发起者与审批者不相交（交集：%s）" % (c, sorted(set(initers) & set(approvers)) or "空"))
    check(pairs_ok, "所有 .init 点都有 .approve 配对")

    print("[6] 防锁死：系统管理员的管理域权限不可剥夺")
    sys_points = [p["code"] for p in points if p["module"] == "系统管理"]
    weak = [c for c in sys_points if any(seed[c][sr] != "ALL" for sr in sys_roles)]
    check(not weak, "系统管理员的 %d 个管理域点均为 ALL（不足：%s）" % (len(sys_points), weak or "无"))

    print("")
    print("统计：权限点 %d（业务 %d + 管理域 %d）· 角色 %d（业务 %d + 系统 %d）"
          % (len(points),
             len([p for p in points if p["module"] != "系统管理"]),
             len(sys_points), len(roles), len(biz_roles), len(sys_roles)))

    if failures:
        print("结果：失败 %d 项" % len(failures))
        for f in failures:
            print("  - %s" % f)
        return 1
    print("结果：全部通过（权限规格自洽）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
