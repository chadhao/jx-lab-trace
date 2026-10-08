#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Markdown 结构与一致性门禁（补充 check_md_tables.py 覆盖不到的两类静默缺陷）。

`check_md_tables.py` 只管**表格列数**。它管不了两类「看起来没问题」的结构/一致性缺陷：

  A. **标题粘连** —— ATX 标题标记 `#{1,6}` 被粘到**上一行行尾**（本应独占一行），
     渲染时结构被破坏（标题不再是标题），但文本仍可读 → 肉眼极难发现。
     由 PM 在 `#53` 自纠时发现，既有门禁查不出。

  B. **版本标记三处不一致** —— 「① 文首指针 / ② §1 文档信息 / ③ 变更记录（最新行）」
     三处版本号不同（同日发生两次：`#52` V1.9→V1.10、`#53` V1.10→V1.12）。
     对应 README **定案 #59**：**同一版本号写在多处，改一处必同步其余**。

★ 为什么**不并入** `check_md_tables.py`：
  「表格列数」门禁是**要求常绿**的基线门禁；而本门禁可能在语料里**确实**报出既存的
  版本漂移（当前 `docs/12` 即有一例）—— 若合并，会让基线门禁常年见红、失去信号。
  故独立成脚本，单独跑、单独判读。

★★ 反面教训（README 定案 #54：**命中总数不能作为结论，必须抽查命中样本**）：
  ① 单号占位符 `####` 里的 `###` 曾被当成「三号标题」→ 一次扫描产出 **57 处幻影缺陷**；
  ② 文档里**写了一行 grep 命令**（`` grep -n '^### 3.13' ``）也被命中；
  ③ 派单方给的验证命令本身可能有洞（同一族）。
  → 本脚本据此做了三重防护，并**逐条打印命中样本**（行号 + 原文），供人核，而非只给计数：
    · 行内代码（`` `…` ``）与围栏代码块（```` ``` ````/`~~~`）内的 `#` 一律**不判**（防 ②）；
    · 命中必须**前置字符为「内容字符」**——`「`/`"`/`'`/`(`/`{`/`[`/`-`/`*`/`>`/`^` 等
      「开引号 / 开括号 / 占位符前缀」上下文一律**不判**（防 ①，`####` 恒被反引号或 `「」` 包裹）；
    · 命中必须 **`#` 游程紧跟空白再跟非空白**（真正的标题形态），排除 `C#`、`#1` 这类词内 `#`。

用法：
    python scripts/check_md_structure.py [docs目录]      # 默认自动定位 <repo>/docs
退出码：0 = 未发现结构/一致性缺陷；1 = 发现缺陷（**逐条打印样本**）；2 = 环境异常（找不到 docs）
"""
import os
import re
import sys

# ----------------------------------------------------------------------------
# 通用
# ----------------------------------------------------------------------------
VER_RE = re.compile(r"V\d+(?:\.\d+)*")
INFO_VER_RE = re.compile(r"^\|\s*版本\s*\|(.*?)\|", re.M)          # §1 文档信息「| 版本 | … |」
HEADER_VER_RE = re.compile(r"本文已[^）]{0,40}?（(V\d+(?:\.\d+)*)）")  # 文首指针「本文已…（Vx.y）」
CHG_HEAD_RE = re.compile(r"^##+\s*\d*\.?\s*变更记录", re.M)            # 「## N. 变更记录」
CHG_ROW_RE = re.compile(r"^\|\s*(V\d+(?:\.\d+)*)\s*\|")               # 变更记录行首版本单元格

# 命中标题粘连时，**前置字符**落在这些「开引号 / 开括号 / 占位符前缀」上下文里则**不判**。
# 依据：真黏连＝「上一行内容 + `###`」，前置字符是**内容字符**（汉字 / 数字 / 字母 / 句读）；
# 而单号占位符 `####` 恒被反引号、`「」`、`{ }`、`-`（`YYMM-####`）等包裹，前置字符是**开引号类**。
OPENER_CHARS = set("`\"'「『（(【[{-*>^…—")  # 开引号 / 开括号 / 占位符前缀

# 明确「不判」的句读（行尾标点）——注释与代码用；当前实现把句读视为内容字符（见 _is_content_char）。
_SENTENCE_PUNCT = "、，。：；！？"


def _is_content_char(ch: str) -> bool:
    """前置字符是否为「内容字符」（→ 疑似真黏连）。

    ★ 刻意**排除**开引号 / 开括号 / 占位符前缀（`OPENER_CHARS`）—— 这些正是 `####`
      单号占位符的上下文（防 README 定案 #54 的「57 处幻影缺陷」重演）。
    ★ **保留**句读（`。` `，` 等）为内容字符：真黏连的典型形态是
      「上一行以句读收尾 + 换行丢失 + `### 标题`」，若把句读也排除会漏检真缺陷。
    """
    if ch in OPENER_CHARS:
        return False
    return not ch.isspace() and ch != "#"


def mask_inline_code(line: str) -> str:
    """把行内代码段（反引号 `…`）整体替换为等长空格，长度不变以便报列号。"""
    out = []
    inside = False
    for ch in line:
        if ch == "`":
            inside = not inside
            out.append(" ")
        else:
            out.append(" " if inside else ch)
    return "".join(out)


GLUE_RE = re.compile(r"([^\s#])(#{1,6})(?=[ \t]+\S)")


def find_glued_headings(lines: list) -> list:
    """返回 [(行号, 列号, 原文)] —— ATX 标题标记被粘到行内（非行首）。"""
    hits = []
    in_fence = False
    fence_tok = None
    for idx, raw in enumerate(lines, 1):
        stripped = raw.lstrip()
        # 围栏代码块开/闭
        m = re.match(r"^(`{3,}|~{3,})", stripped)
        if m:
            tok = m.group(1)[0]
            if not in_fence:
                in_fence, fence_tok = True, tok
            elif tok == fence_tok:
                in_fence, fence_tok = False, None
            continue
        if in_fence:
            continue
        masked = mask_inline_code(raw)
        for mm in GLUE_RE.finditer(masked):
            pre = mm.group(1)
            if not _is_content_char(pre):
                continue
            hits.append((idx, mm.start(2) + 1, raw.rstrip()))
    return hits


# ----------------------------------------------------------------------------
# Check B：版本标记三处一致
# ----------------------------------------------------------------------------
def _max_ver(vers: list):
    def key(v):
        return tuple(int(x) for x in v.lstrip("V").split("."))
    return max(vers, key=key) if vers else None


def collect_version_markers(text: str) -> dict:
    """返回 {'header':…, 'info':…, 'changelog':…}（缺者为 None）。"""
    out = {"header": None, "info": None, "changelog": None}

    m = HEADER_VER_RE.search(text)
    if m:
        out["header"] = m.group(1)

    m = INFO_VER_RE.search(text)
    if m:
        vm = VER_RE.search(m.group(1))
        if vm:
            out["info"] = vm.group(0)

    chg_head = CHG_HEAD_RE.search(text)
    if chg_head:
        rest = text[chg_head.end():]
        nxt = re.search(r"^##\s", rest, re.M)
        sec = rest[: nxt.start()] if nxt else rest
        vers = [mm.group(1) for mm in (CHG_ROW_RE.match(l) for l in sec.split("\n")) if mm]
        out["changelog"] = _max_ver(vers)
    return out


def check_version_consistency(fn: str, text: str) -> list:
    """返回 [(标记名, 版本)] 当三处不一致时；否则 []。"""
    mk = collect_version_markers(text)
    present = {k: v for k, v in mk.items() if v}
    if len(present) < 2:
        return []  # 只有 0/1 处版本标记 → 无从比对（不臆测）
    distinct = set(present.values())
    if len(distinct) <= 1:
        return []
    return sorted(present.items())


# ----------------------------------------------------------------------------
# 主流程
# ----------------------------------------------------------------------------
def locate_docs() -> str | None:
    here = os.path.dirname(os.path.abspath(__file__))
    candidates = [
        os.path.join(os.path.dirname(here), "docs"),  # <repo>/scripts/ 同级 docs/
        os.path.join(here, "docs"),
        "docs",
    ]
    return next((c for c in candidates if os.path.isdir(c)), None)


def main() -> int:
    root = sys.argv[1] if len(sys.argv) > 1 else locate_docs()
    if root is None or not os.path.isdir(root):
        print("FAIL 找不到 docs/ 目录；用法：python scripts/check_md_structure.py [docs目录]")
        return 2

    scanned = 0
    glue_total = 0
    ver_total = 0
    ver_checked = 0

    for dirpath, _dirnames, filenames in os.walk(root):
        for fn in sorted(filenames):
            if not fn.endswith(".md"):
                continue
            scanned += 1
            path = os.path.join(dirpath, fn)
            rel = os.path.relpath(path).replace("\\", "/")
            with open(path, "r", encoding="utf-8", newline="") as f:
                text = f.read()
            lines = text.splitlines()

            # A. 标题粘连
            for ln, col, src in find_glued_headings(lines):
                glue_total += 1
                print(f"[A-标题粘连] {rel}:{ln}:{col}  {src}")

            # B. 版本三处一致
            mk = collect_version_markers(text)
            if sum(1 for v in mk.values() if v) >= 2:
                ver_checked += 1  # 至少两处版本标记存在 → 可交叉比对
            bad = check_version_consistency(fn, text)
            if bad:
                ver_total += 1
                detail = " · ".join(f"{k}={v}" for k, v in bad)
                print(f"[B-版本不一致] {rel}  三处版本标记不同：{detail}")

    if scanned == 0:
        print(f"FAIL {root} 下没有 .md 文件；门禁未实际检查任何内容")
        return 2

    if glue_total or ver_total:
        print(
            f"\n共 {glue_total + ver_total} 处问题"
            f"（标题粘连 {glue_total}；版本不一致 {ver_total}）"
            f"；已扫 {scanned} 个文件，其中 {ver_checked} 个具备可比对的多处版本标记"
        )
        return 1

    print(
        f"OK 未发现标题粘连 / 版本标记不一致"
        f"（已扫 {scanned} 个文件，其中 {ver_checked} 个具备可比对的版本标记）"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
