#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
check_spec.py —— spec 判据双引擎的 **Python 侧**（N-023 D1.3）。

★ 双引擎：本脚本与 internal/specload（Go 侧）各自独立实现 checks.json#kinds
  全部 kind 的求值器，对**同一份** spec/checks.json 求值。
  任一引擎不支持某 kind（新增 kind 未两侧同步）或任一判据不过 ⇒ 退出码 1。
★ 一致性判据（D5-6）：两引擎必须消费同一 kinds 全集与同一 checks 集 ——
  本脚本会校验 kinds 声明与逐条 kind 归属；Go 侧 specload_test 做同样断言。

消费的 spec 文件：
  spec/checks.json     判据清单
  spec/schema.sql      DDL（表/列/唯一键/索引）
  spec/code-rules.json 码规则（对象类型）
  spec/enums.json      状态枚举
  spec/forms/*.json    表单规格
  internal/httpapi/recv.go 等源码（source_regex / route_registered）

退出码：0 = 全部通过；1 = 有失败/未知 kind。
"""
import io
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
SPEC = os.path.join(ROOT, "spec")

failures = []
pass_count = 0


def ok(check_id, desc):
    global pass_count
    pass_count += 1
    print("  ok   %s %s" % (check_id, desc))


def fail(check_id, desc, detail):
    failures.append("%s %s —— %s" % (check_id, desc, detail))
    print("  FAIL %s %s —— %s" % (check_id, desc, detail))


# ---------- schema.sql 解析 ----------

def parse_schema(sql_text):
    """返回 {table: {'columns': set, 'unique_keys': {name: [cols]}, 'indexes': {name: [cols]}}}"""
    tables = {}
    # 去注释行（-- 开头），保留语句
    lines = []
    for line in sql_text.split("\n"):
        if line.strip().startswith("--"):
            continue
        lines.append(line)
    text = "\n".join(lines)
    for m in re.finditer(r"(?is)CREATE\s+TABLE\s+(\w+)\s*\((.*?)\)\s*(ENGINE|;)", text):
        name = m.group(1).lower()
        body = m.group(2)
        cols = set()
        uniq = {}
        idx = {}
        for raw in body.split("\n"):
            line = raw.strip().rstrip(",")
            if not line:
                continue
            mu = re.match(r"(?i)UNIQUE\s+KEY\s+(\w+)\s*\(([^)]*)\)", line)
            if mu:
                uniq[mu.group(1).lower()] = [c.strip().strip("`") for c in mu.group(2).split(",")]
                continue
            mi = re.match(r"(?i)KEY\s+(\w+)\s*\(([^)]*)\)", line)
            if mi:
                idx[mi.group(1).lower()] = [c.strip().strip("`") for c in mi.group(2).split(",")]
                continue
            mc = re.match(r"(?i)^(`?\w+`?)\s+[A-Z]", line)
            if mc and line.upper().split()[0] not in ("PRIMARY", "CONSTRAINT", "FOREIGN", "CHECK"):
                col = mc.group(1).strip("`").lower()
                if col not in ("unique", "key", "primary", "fulltext", "spatial", "index"):
                    cols.add(col)
        tables[name] = {"columns": cols, "unique_keys": uniq, "indexes": idx}
    return tables


# ---------- kind 求值器 ----------

def eval_check(chk, ctx):
    kind = chk["kind"]
    cid, desc = chk["id"], chk.get("desc", "")

    if kind == "table_count":
        n = len(ctx["schema"])
        if n == chk["expect"]:
            ok(cid, desc)
        else:
            fail(cid, desc, "表数 %d != 期望 %d" % (n, chk["expect"]))

    elif kind == "schema_table_exists":
        if chk["table"].lower() in ctx["schema"]:
            ok(cid, desc)
        else:
            fail(cid, desc, "表 %s 不存在" % chk["table"])

    elif kind == "schema_unique_key":
        t = ctx["schema"].get(chk["table"].lower())
        if not t:
            fail(cid, desc, "表 %s 不存在" % chk["table"])
            return
        want = [c.lower() for c in chk["columns"]]
        found = any(v == want for v in t["unique_keys"].values())
        if found:
            ok(cid, desc)
        else:
            fail(cid, desc, "唯一键 %s 不存在（现有: %s）" % (want, list(t["unique_keys"].values())))

    elif kind == "schema_column_present":
        t = ctx["schema"].get(chk["table"].lower())
        if not t:
            fail(cid, desc, "表 %s 不存在" % chk["table"])
        elif chk["column"].lower() in t["columns"]:
            ok(cid, desc)
        else:
            fail(cid, desc, "列 %s 不存在" % chk["column"])

    elif kind == "schema_column_absent":
        t = ctx["schema"].get(chk["table"].lower())
        if not t:
            fail(cid, desc, "表 %s 不存在" % chk["table"])
        elif chk["column"].lower() not in t["columns"]:
            ok(cid, desc)
        else:
            fail(cid, desc, "列 %s 应已删除但仍在" % chk["column"])

    elif kind == "schema_index_present":
        t = ctx["schema"].get(chk["table"].lower())
        if not t:
            fail(cid, desc, "表 %s 不存在" % chk["table"])
            return
        name = chk["index"].lower()
        want = [c.lower() for c in chk.get("columns", [])]
        got = t["indexes"].get(name)
        if got is not None and (not want or got == want):
            ok(cid, desc)
        else:
            fail(cid, desc, "索引 %s 不存在或列不符（现有: %s）" % (chk["index"], t["indexes"]))

    elif kind == "schema_index_absent":
        t = ctx["schema"].get(chk["table"].lower())
        if not t:
            fail(cid, desc, "表 %s 不存在" % chk["table"])
        elif chk["index"].lower() not in t["indexes"]:
            ok(cid, desc)
        else:
            fail(cid, desc, "索引 %s 应已移除但仍在" % chk["index"])

    elif kind == "code_rules_object_type":
        letter = chk["letter"]
        ot = ctx["code_rules"].get("object_types", {}).get(letter)
        if not ot:
            fail(cid, desc, "对象类型 %s 未登记" % letter)
        elif ot.get("name") != chk["name"]:
            fail(cid, desc, "名称 %r != 期望 %r" % (ot.get("name"), chk["name"]))
        else:
            ok(cid, desc)

    elif kind == "enum_values":
        e = ctx["enums"].get("enums", {}).get(chk["enum"])
        if not e:
            fail(cid, desc, "枚举 %s 不存在" % chk["enum"])
        elif e.get("values") != chk["values"]:
            fail(cid, desc, "%s != %s" % (e.get("values"), chk["values"]))
        else:
            ok(cid, desc)

    elif kind == "form_fields_present":
        f = ctx["forms"].get(chk["form"])
        if not f:
            fail(cid, desc, "表单 %s 不存在" % chk["form"])
            return
        have = {x["key"] for x in f.get("fields", [])}
        missing = [k for k in chk["fields"] if k not in have]
        if missing:
            fail(cid, desc, "缺字段 %s" % missing)
        else:
            ok(cid, desc)

    elif kind == "form_fields_absent":
        f = ctx["forms"].get(chk["form"])
        if not f:
            fail(cid, desc, "表单 %s 不存在" % chk["form"])
            return
        have = {x["key"] for x in f.get("fields", [])}
        bad = [k for k in chk["fields"] if k in have]
        if bad:
            fail(cid, desc, "不应存在的字段 %s" % bad)
        else:
            ok(cid, desc)

    elif kind == "source_regex":
        path = os.path.join(ROOT, chk["file"])
        try:
            s = io.open(path, encoding="utf-8").read()
        except OSError as e:
            fail(cid, desc, "读不到 %s: %s" % (chk["file"], e))
            return
        if re.search(chk["pattern"], s):
            ok(cid, desc)
        else:
            fail(cid, desc, "正则未命中: %s" % chk["pattern"])

    elif kind == "source_absent_regex":
        path = os.path.join(ROOT, chk["file"])
        try:
            s = io.open(path, encoding="utf-8").read()
        except OSError as e:
            fail(cid, desc, "读不到 %s: %s" % (chk["file"], e))
            return
        if not re.search(chk["pattern"], s):
            ok(cid, desc)
        else:
            fail(cid, desc, "正则命中（应不存在）: %s" % chk["pattern"])

    elif kind == "route_registered":
        path = os.path.join(ROOT, "internal", "httpapi", "recv.go")
        s = io.open(path, encoding="utf-8").read()
        # 路由在 e.Group("/api/recv") 下以相对路径声明（mountReceiving 集中注册）
        rel = chk["path"]
        prefix = "/api/recv"
        if rel.startswith(prefix):
            rel = rel[len(prefix):]
        pat = r'g\.%s\("%s"' % (re.escape(chk["method"]), re.escape(rel))
        if re.search(pat, s):
            ok(cid, desc)
        else:
            fail(cid, desc, "路由 %s %s 未在 recv.go 注册" % (chk["method"], chk["path"]))

    else:
        fail(cid, desc, "未知 kind %r —— 双引擎需同批支持（kind 全集声明: %s）"
             % (kind, ctx["kinds"]))


def main():
    checks_path = os.path.join(SPEC, "checks.json")
    doc = json.load(io.open(checks_path, encoding="utf-8"))
    kinds_declared = doc.get("kinds", [])

    print("[spec-checks] %d 条判据 · kinds 声明 %d 种" % (len(doc["checks"]), len(kinds_declared)))

    # kinds 全集与逐条 kind 必须自洽（防「声明了 kind 却无判据」或「判据用未声明 kind」）
    used = {c["kind"] for c in doc["checks"]}
    undeclared = used - set(kinds_declared)
    if undeclared:
        fail("KINDS", "判据使用了未声明的 kind", str(sorted(undeclared)))

    schema_sql = io.open(os.path.join(SPEC, "schema.sql"), encoding="utf-8").read()
    forms = {}
    forms_dir = os.path.join(SPEC, "forms")
    for fn in sorted(os.listdir(forms_dir)):
        if fn.endswith(".json"):
            f = json.load(io.open(os.path.join(forms_dir, fn), encoding="utf-8"))
            forms[f["form"]] = f

    ctx = {
        "kinds": kinds_declared,
        "schema": parse_schema(schema_sql),
        "code_rules": json.load(io.open(os.path.join(SPEC, "code-rules.json"), encoding="utf-8")),
        "enums": json.load(io.open(os.path.join(SPEC, "enums.json"), encoding="utf-8")),
        "forms": forms,
    }

    for chk in doc["checks"]:
        eval_check(chk, ctx)

    print("")
    if failures:
        print("结果：失败 %d 项（通过 %d）" % (len(failures), pass_count))
        for f in failures:
            print("  - %s" % f)
        return 1
    print("结果：全部通过（%d 项判据 · Python 引擎）" % pass_count)
    return 0


if __name__ == "__main__":
    sys.exit(main())
