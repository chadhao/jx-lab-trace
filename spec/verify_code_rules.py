#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
追踪码机读规格自校验（无第三方依赖）。

用途：校验 spec/code-rules.json 自身是否自洽，尤其是
  ① 各段宽度之和 == payload 长度
  ② 示例向量的校验位能否用规格里声明的算法复算出来
  ③ full == payload + check；human 与 full 严格同构（可双向转换）

★ 为什么必须自校验：校验位算错属于「看起来对、用起来才发现」的那类缺陷。
  示例向量一旦进规格，就成了实现方的固定回归用例 —— 它必须自己是对的。

退出码：0 = 全部通过；1 = 有断言失败。
"""
import io
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SPEC = os.path.join(HERE, "code-rules.json")

failures = []


def check(cond, msg):
    if cond:
        print("  ok   %s" % msg)
    else:
        print("  FAIL %s" % msg)
        failures.append(msg)


def compute_check(payload, alphabet, weights, modulus):
    """按规格声明的算法计算校验字符。"""
    total = 0
    for k, ch in enumerate(reversed(payload)):
        w = weights[k % len(weights)]
        total += alphabet.index(ch) * w
    return alphabet[total % modulus], total


def to_human(full, widths):
    """把裸串切成带分隔符的人读行。"""
    out = []
    i = 0
    for w in widths:
        out.append(full[i:i + w])
        i += w
    return "-".join(out)


def main():
    with io.open(SPEC, encoding="utf-8") as f:
        spec = json.load(f)

    alphabet = spec["alphabet"]
    ca = spec["check_algorithm"]
    weights = ca["weights"]
    modulus = ca["modulus"]
    segments = spec["segments"]
    widths = [s["width"] for s in segments]
    human_groups = spec["code_faces"]["human_groups"]

    print("[1] 长度与字母表")
    check(ca["modulus"] == len(alphabet),
          "modulus(%d) == 字母表长度(%d)" % (ca["modulus"], len(alphabet)))
    seg_sum = sum(widths)
    check(seg_sum == spec["length"]["payload"],
          "段宽之和(%d) == payload 长度(%d)" % (seg_sum, spec["length"]["payload"]))
    check(spec["length"]["payload"] + spec["length"]["check"] == spec["length"]["total"],
          "payload(%d) + check(%d) == total(%d)"
          % (spec["length"]["payload"], spec["length"]["check"], spec["length"]["total"]))
    check(sum(human_groups) == spec["length"]["total"],
          "human_groups 之和(%d) == total(%d)" % (sum(human_groups), spec["length"]["total"]))

    print("[2] 段定义与对象类型")
    for s in segments:
        if "pattern" in s:
            try:
                re.compile(s["pattern"])
                ok = True
            except re.error:
                ok = False
            check(ok, "段 %s pattern 可编译：%s" % (s["key"], s["pattern"]))
        elif "enum" in s:
            ok = all(len(v) == s["width"] and all(c in alphabet for c in v)
                     for v in s["enum"])
            check(ok, "段 %s enum 取值合法且宽度匹配：%s" % (s["key"], s["enum"]))
        else:
            check(False, "段 %s 必须声明 pattern 或 enum" % s["key"])

    print("[2b] 不变量：三段序 == 三层嵌套")
    for t, d in spec["object_types"].items():
        check(t in "ABCDE", "对象类型 %s 合法" % t)
        depth = d["depth"]
        check(depth in (1, 2, 3), "对象类型 %s 的 depth 合法" % t)
        seqs = [d["seq1"], d["seq2"], d["seq3"]]
        filled = [i for i, v in enumerate(seqs) if v != "000"]
        check(filled == list(range(depth)),
              "%s(%s) depth=%d，实义层索引=%s → 与 depth 一致"
              % (t, d["name"], depth, filled))

    print("[3] 示例向量复算（核心）")
    for v in spec["sample_vectors"]:
        payload = v["payload"]
        check(len(payload) == spec["length"]["payload"],
              "%s payload 长度 == %d" % (v["object"], spec["length"]["payload"]))
        got_check, got_sum = compute_check(payload, alphabet, weights, modulus)
        check(got_sum == v["weighted_sum"],
              "%s 加权和 %d == 声明值 %d" % (v["object"], got_sum, v["weighted_sum"]))
        check(got_check == v["check"],
              "%s 校验位 %s == 声明值 %s" % (v["object"], got_check, v["check"]))
        check(payload + got_check == v["full"],
              "%s full == payload + check" % v["object"])
        check(to_human(v["full"], human_groups) == v["human"],
              "%s human == 按 human_groups 拼出的人读行" % v["object"])
        # 反解：人读行去掉分隔符必须还原成 full
        check(v["human"].replace("-", "") == v["full"],
              "%s 人读行去分隔符可还原 full（严格同构）" % v["object"])

    print("[4] 相邻换位可检出性（算法性质抽验）")
    v = spec["sample_vectors"][4]
    payload = v["payload"]
    det = 0
    tried = 0
    for i in range(len(payload) - 1):
        if payload[i] == payload[i + 1]:
            continue
        swapped = payload[:i] + payload[i + 1] + payload[i] + payload[i + 2:]
        tried += 1
        if compute_check(swapped, alphabet, weights, modulus)[0] != compute_check(payload, alphabet, weights, modulus)[0]:
            det += 1
    check(tried > 0, "存在可测的相邻换位样本（%d 组）" % tried)
    check(det == tried, "相邻换位 %d/%d 全部可检出" % (det, tried))

    print("")
    if failures:
        print("结果：失败 %d 项" % len(failures))
        for f in failures:
            print("  - %s" % f)
        return 1
    print("结果：全部通过（规格自洽，示例向量可复算）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
