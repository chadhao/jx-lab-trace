#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
check_mimo_stall.py —— 判断 mimo 轮次是「在跑」还是「卡住了」。

★ 为什么需要它：2026-10-08 那次 mimo **空转 6 小时**才被发现 ——
  它的特征不是"进程没了"，而是 **进程活着、但日志早已停止增长、也没有任何外连**。
  ⇒ ★ 判据：**「进程存活」不等于「在推进」；要看「客观产物有没有增长」。**

判据（三项合起来才判停滞，避免误杀）：
  ① 日志 **mtime 距今 > --max-idle**（默认 900 秒 = 15 分钟）
  ② 给定 --pid 时，该进程**仍然存活**（进程已退出 ⇒ 那是"结束"，不是"卡住"）
  ③ ★ 只判停滞，**不自动杀** —— 处置交给调用方（本脚本只负责"看得见"）

用法：
  python scripts/check_mimo_stall.py                       # 用默认日志与阈值
  python scripts/check_mimo_stall.py --log /tmp/x.log --max-idle 600 --pid 16884

退出码：0 = 活跃或无轮次（**不算失败**）；1 = 判定停滞。
"""
import argparse
import os
import subprocess
import sys
import tempfile
import time

# ★★ 路径陷阱（本项目实测踩过）：Git Bash 的 `/tmp` 会被 MSYS **转换成 Windows 的 %TEMP%**
#    之后再传给本 Python（Windows 程序）；但**写死在源码里的 `/tmp/...` 不经过转换**，
#    Python 会把它解析成 `C:\tmp\...`（不存在）⇒ 判据恒报"无轮次"（**假绿**）。
#    ⇒ 默认值一律用 `tempfile.gettempdir()`（＝ %TEMP%，与 MSYS 的 /tmp 同一处）。
DEFAULT_LOG = os.path.join(tempfile.gettempdir(), "drive_mimo_N_001.log")


def normalize(path: str) -> str:
    """把 MSYS 风格的 /tmp/... 归一到 Windows 的 %TEMP%\\...，避免两个世界各说各话。"""
    if path.startswith("/tmp/") or path == "/tmp":
        rest = path[len("/tmp"):].lstrip("/\\")
        return os.path.join(tempfile.gettempdir(), rest) if rest else tempfile.gettempdir()
    return path


def pid_alive(pid: int) -> bool:
    """跨平台探活：Windows 用 tasklist，其它用 ps。

    ★★ 编码陷阱（本项目实测踩过）：中文 Windows 上 `tasklist` 输出是 **GBK**，不是 UTF-8。
       若用 `text=True`（按 locale/UTF-8 解码）会**抛 UnicodeDecodeError**，
       被 `except` 吞掉后 **把活着的进程判成"已退出"** ⇒ 卡住的轮次会被误判为"正常结束"
       —— **这是最坏的一类假绿**。
       ⇒ 必须拿 **bytes**、用 `errors="ignore"` 解码（PID 是 ASCII，不会因忽略而丢）。
    """
    try:
        if os.name == "nt" or sys.platform.startswith("win"):
            raw = subprocess.run(["tasklist", "/FI", "PID eq %d" % pid],
                                 capture_output=True, timeout=10).stdout
            out = raw.decode("utf-8", errors="ignore")
            if str(pid) not in out:
                out = raw.decode("gbk", errors="ignore")
            return str(pid) in out
        raw = subprocess.run(["ps", "-p", str(pid)],
                             capture_output=True, timeout=10).stdout
        out = raw.decode("utf-8", errors="ignore")
        return str(pid) in out.split("\n", 1)[-1]
    except Exception:
        return False


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--log", default=DEFAULT_LOG)
    ap.add_argument("--max-idle", type=int, default=900, help="日志静止多少秒判停滞")
    ap.add_argument("--pid", type=int, default=0, help="mimo 进程号（给了就一并核存活）")
    a = ap.parse_args()
    a.log = normalize(a.log)

    if not os.path.isfile(a.log):
        print("○ 无轮次：日志不存在（%s）—— 视为『没在跑』，非停滞" % a.log)
        return 0

    now = time.time()
    mtime = os.path.getmtime(a.log)
    size = os.path.getsize(a.log)
    idle = int(now - mtime)
    print("日志：%s" % a.log)
    print("大小：%d 字节 ｜ 最后写入：%s ｜ 已静止：%d 秒（阈值 %d）"
          % (size, time.strftime("%F %T", time.localtime(mtime)), idle, a.max_idle))

    alive = None
    if a.pid:
        alive = pid_alive(a.pid)
        print("进程：PID %d ⇒ %s" % (a.pid, "存活" if alive else "**已退出**"))

    if idle <= a.max_idle:
        print("OK 活跃（日志在阈值内仍有写入）")
        return 0

    # 日志已静止 —— 再看进程状态，区分「卡住」与「已结束」
    if a.pid and alive is False:
        print("○ 判定：**轮次已结束**（进程已退出，日志静止属正常终态）—— 非停滞")
        return 0

    print("")
    print("● 判定：**停滞** —— 日志已静止 %d 秒%s" %
          (idle, "，而进程仍存活（典型空转特征）" if alive else ""))
    print("")
    print("★ 处置建议（由调用方执行，本脚本不自动动手）：")
    print("  1. 核对是否真无产出：`git status --porcelain`、`git log --oneline -1`；")
    print("  2. 确认无外连（Windows：`netstat -ano | findstr <pid>` 应无 ESTABLISHED）；")
    print("  3. 终止空转轮次与残留子进程（ssh 隧道等），再重新派工；")
    print("  4. ★ 重新派工前先走 `COLLAB.md §0 铁律 8` 的「报障前三问」。")
    return 1


if __name__ == "__main__":
    sys.exit(main())
