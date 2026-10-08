#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
check_local_debug.py —— 「本机调试残留」提醒（**会报**级，不阻塞总判定）。

★ 依据用户 2026-10-09 的硬约束：**系统调试一律在测试服务器上进行，不在本机进行**。
  见 docs/05-环境与调试约定.md。

★ 为什么放「会报」而不是「必绿」：**本机是否在跑进程是环境事实，不是代码事实** ——
  用必绿去卡它，会把「别人本机状态」变成阻断提交的理由。
  ★ 但**不许静默**：一旦发现，就明确列出来提醒人去处置。

判据（静态、可移植）：
  仓库根出现下列**只会在"本机起过服务"时才产生**的产物 ⇒ 报出：
    · smoke.out / smoke.err（本地 smoke 测试输出）
    · *.pid（本地跑服务写的 PID 文件）
    · bin/*.exe（Windows 本地可执行 —— 允许编译，但若存在则提示"本机勿运行"）
  另：提示当前服务器部署端口与约定，便于人核对。

退出码：0 = 无残留；1 = 有残留（会报，不阻塞）。
"""
import glob
import io
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

ARTIFACT_RULES = [
    ("smoke.out", "本机 smoke 测试输出"),
    ("smoke.err", "本机 smoke 测试输出"),
    ("*.pid", "本机跑服务写的 PID 文件"),
]


def main():
    hits = []

    for pattern, why in ARTIFACT_RULES:
        for p in sorted(glob.glob(os.path.join(ROOT, pattern))):
            hits.append((os.path.relpath(p, ROOT), why))

    exes = sorted(glob.glob(os.path.join(ROOT, "bin", "*.exe")))
    for p in exes:
        hits.append((os.path.relpath(p, ROOT), "本机编译的 Windows 可执行（★ 允许编译，但**不得在本机运行**）"))

    print("已扫：仓库根 + bin/")
    print("约定：本机只做 编辑 / 编译 / 纯单测 / 静态检查；"
          "**要 listen 或要改远端状态的，一律在测试服务器（192.168.10.50，端口 127.0.0.1:18080）上做**")
    print("部署命令：bash scripts/deploy-test-server.sh --restart --smoke")

    if not hits:
        print("OK 未发现本机调试残留")
        return 0

    print("")
    print("● 发现本机调试残留 %d 项 —— **请人工处置（不阻塞总判定）**：" % len(hits))
    for path, why in hits:
        print("  · %s   — %s" % (path, why))
    print("")
    print("★ 处置建议：删掉这些产物；后续调试一律改用 `scripts/deploy-test-server.sh` 在服务器上做。")
    return 1


if __name__ == "__main__":
    sys.exit(main())
