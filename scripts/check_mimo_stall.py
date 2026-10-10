#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
check_mimo_stall.py —— 判断 mimo 轮次是「在跑」还是「卡住了」。

★ 为什么需要它：2026-10-08 那次 mimo **空转 6 小时**才被发现 ——
  它的特征不是"进程没了"，而是 **进程活着、但日志早已停止增长、也没有任何外连**。
  ⇒ ★ 判据：**「进程存活」不等于「在推进」；要看「客观产物有没有增长」。**

判据（**四项合起来才判停滞**，避免误杀）：
  ① 日志 **mtime 距今 > --max-idle**（默认 900 秒 = 15 分钟）
  ② 给定 --pid 时，该进程**仍然存活**（进程已退出 ⇒ 那是"结束"，不是"卡住"）
  ③ ★★ **CPU 时间在采样窗口内【没有】增长**（2026-10-10 加，见下）
  ④ ★ 只判停滞，**不自动杀** —— 处置交给调用方（本脚本只负责"看得见"）

★★ 为什么加 ③（**实测事故驱动**）：2026-10-10 派工 `N-016`，mimo **连续 3 次 attempt 都在
   ~15 分钟处被判"停滞"并被终止**，而日志显示它当时**正在正常干活**（改完 CSS、静态判据三项全过、
   接着加载 playwright 做**浏览器级深色偏好验收**）。⇒ **"不写日志" ≠ "没在干活"** ——
   长任务（浏览器验收 / 长推理 / 等外部 IO）本来就可能十几分钟不产日志。
   ⇒ ★★ **判据不是越严越好，而是越准越好：误伤合法用法 ⇒ 对方理性的应对是绕过它 ⇒ 等于判据不存在。**
   ⇒ 补一条**客观证据**：**进程 CPU 时间在采样窗口内增长 ⇒ 它在干活 ⇒ 不判停滞。**

用法：
  python scripts/check_mimo_stall.py                       # 用默认日志与阈值
  python scripts/check_mimo_stall.py --log /tmp/x.log --max-idle 600 --pid 16884

退出码：0 = 活跃或无轮次（**不算失败**）；1 = 判定停滞。
"""
import argparse
import os
import re
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


def cpu_seconds(pid: int):
    """进程累计 CPU 时间（秒）；取不到 ⇒ None。跨平台。

    ★ 为什么用它：**"不写日志" ≠ "没在干活"** —— 长任务（浏览器验收/长推理/等 IO）
      本就会长时间不产日志。CPU 时间在增长是最直接的"它在推进"证据。
    ★ 编码同 pid_alive：中文 Windows 的 tasklist 输出是 GBK。
    """
    try:
        if os.name == "nt" or sys.platform.startswith("win"):
            raw = subprocess.run(["tasklist", "/v", "/FI", "PID eq %d" % pid],
                                 capture_output=True, timeout=20).stdout
            txt = raw.decode("utf-8", errors="ignore")
            if not re.search(r"\d+:\d{2}:\d{2}", txt):
                txt = raw.decode("gbk", errors="ignore")
        else:
            raw = subprocess.run(["ps", "-p", str(pid), "-o", "cputime="],
                                 capture_output=True, timeout=10).stdout
            txt = raw.decode("utf-8", errors="ignore")
        m = re.search(r"(?:(\d+)-)?(\d+):(\d{2}):(\d{2})", txt)
        if not m:
            return None
        d, h, mi, se = (int(x) if x else 0 for x in m.groups())
        return d * 86400 + h * 3600 + mi * 60 + se
    except Exception:
        return None


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--log", default=DEFAULT_LOG)
    ap.add_argument("--max-idle", type=int, default=900, help="日志静止多少秒判停滞")
    ap.add_argument("--pid", type=int, default=0, help="mimo 进程号（给了就一并核存活）")
    ap.add_argument("--cpu-probe", type=int, default=0, metavar="PID",
                    help="只做一次 CPU 采样并打印秒数（供外部脚本两次对比用）；不参与停滞判定")
    ap.add_argument("--cpu-sample-sec", type=int, default=5,
                    help="CPU 采样窗口秒数（窗口内 CPU 有增长 ⇒ 判为在干活，不判停滞）")
    a = ap.parse_args()
    a.log = normalize(a.log)

    # ★ 供 drive_mimo.sh 的看门狗调用：单次采样 CPU 秒数（-1 = 取不到）后立即退出。
    if a.cpu_probe:
        v = cpu_seconds(a.cpu_probe)
        print(-1 if v is None else v)
        return 0

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

    # ★★ 免死条件（2026-10-10 加）：日志静止但 **CPU 在涨** ⇒ 它在干活，不判停滞。
    if a.pid and alive:
        c1 = cpu_seconds(a.pid)
        if c1 is not None:
            time.sleep(max(1, a.cpu_sample_sec))
            c2 = cpu_seconds(a.pid)
            if c2 is not None and c2 > c1:
                print("OK 活跃（日志静止 %d 秒，但 **CPU 时间在增长**：%ds → %ds ⇒ 在干活，非停滞）"
                      % (idle, c1, c2))
                return 0
            print("CPU 时间：%s → %s（窗口 %ds 内无增长）"
                  % (c1, c2, a.cpu_sample_sec))
        else:
            print("CPU 时间：取不到（跳过该免死判据）")

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
