#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
scripts/check_perm_registry.py —— 权限点「注册 ⇄ 消费」一致性门禁（批 1 交付，必绿项）。

★ 动因（docs/01 §8.0.1 硬约束 2，铁律「声明了却没人执行 ＞ 没声明」）：
  权限点字典由代码注册、后台不可增删；那么必须有机检同时守住两端 ——

  ① 消费端：spec/permission-points.json 里**每一个** code，都要在 Go 非测试代码中
     **至少被引用一次**（权限点常量形式）—— 防「配了没人用」的假权限；
  ② 声明端：Go 代码里出现的权限点标识（受保护入口的 RequirePerm 实参、
     Code 常量取值），必须在 spec/permission-points.json 里**已登记** —— 防「没声明却保护了」；
  ③ 禁裸写：受保护入口处**不得**直接写权限点字符串字面量，必须引用常量；
     且常量定义集中在**单一声明文件**（`Code = "..."` 只允许出现在一处）；
  ④ 种子一致：spec 的 seed 与库内 s_role_permission 行数一致（51 × 6 = 306），
     同时核对权限点 51、角色 6。

★ 判据边界（本脚本能保证什么 / 不能保证什么）：
  · 采用**集中注册表方案**：所有受保护入口必须在路由处以
    `RequirePerm(st, permission.XXX)` 形式声明（见 internal/httpapi/server.go 头注释）。
    本脚本静态扫描全部**非测试** .go 文件里出现的 `RequirePerm(...)`：
      - 实参里出现字符串字面量 ⇒ ③ 红；
      - 实参里没有 `permission.Ident` ⇒ ③ 红（无法静态核验的写法一律拒绝 —— 宁可误红，不漏检）；
      - `permission.Ident` 解析出的取值不在 spec ⇒ ② 红。
  · **能保证**：所有写出来的 `RequirePerm(...)` 调用都指向已登记常量；每个 spec code
    至少被声明文件之外的代码引用一次；非声明文件与迁移 SQL 里不出现 code 字面量。
  · **不能保证**：① 无法证明某权限点真的**拦住了东西**（那由运行时测试守：
    TC-M0-01/02/06）；② 若未来绕开 RequirePerm 自造另一套守卫函数，本脚本不认识它
    —— 那属于「新入口没走统一守卫」，需在 code review / 议题层拦，静态门禁到此为止。
  · ④ 库内计数经 `go run ./scripts/jxq` 完成（复用 go-sql-driver，**不给 Python 加
    MySQL 驱动依赖**）；**缺 JX_DB_DSN 或连不上库一律判红** —— 不接受「没连库也说一致」。

用法：
  python scripts/check_perm_registry.py                 # 检查真实仓库（含 ④ 库内）
  python scripts/check_perm_registry.py --selftest      # 用违规样本自证判据有牙齿
  python scripts/check_perm_registry.py --selftest --only <case>
退出码：0 = 通过；1 = 有失败项。
"""
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

DECL_RE = re.compile(r'(\w+)\s+Code\s*=\s*"([^"]+)"')
REQUIRE_PERM_RE = re.compile(r'RequirePerm\s*\(')
CODE_CAST_RE = re.compile(r'\bCode\s*\(\s*"([^"]*)"\s*\)')
STRING_LIT_RE = re.compile(r'"((?:[^"\\]|\\.)*)"')
SPEC_CODE_RE = re.compile(r'^[a-z0-9_]+(\.[a-z0-9_]+)+$')
IDENT_ARG_RE = re.compile(r'permission\.\w+')
SKIP_DIRS = (".git", "node_modules", "bin")


# ---------------------------------------------------------------- 工具

def read(path):
    with io.open(path, encoding="utf-8") as f:
        return f.read()


def write_file(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with io.open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(text)


def _walk(root, suffix, include_tests):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(
            d for d in dirnames
            if d not in SKIP_DIRS and not d.startswith("_") and not d.startswith(".")
        )
        for name in sorted(filenames):
            if not name.endswith(suffix):
                continue
            if suffix == ".go" and not include_tests and name.endswith("_test.go"):
                continue
            yield os.path.join(dirpath, name)


def split_call_args(text):
    """把实参按顶层逗号切开（忽略括号/引号内的逗号）。"""
    args, depth, cur, in_str, escape = [], 0, [], False, False
    for ch in text:
        if in_str:
            cur.append(ch)
            if escape:
                escape = False
            elif ch == "\\":
                escape = True
            elif ch == '"':
                in_str = False
            continue
        if ch == '"':
            in_str = True
            cur.append(ch)
        elif ch == "(":
            depth += 1
            cur.append(ch)
        elif ch == ")":
            depth -= 1
            if depth < 0:
                break
            cur.append(ch)
        elif ch == "," and depth == 0:
            args.append("".join(cur).strip())
            cur = []
        else:
            cur.append(ch)
    tail = "".join(cur).strip()
    if tail:
        args.append(tail)
    return args


def find_require_perm_calls(source):
    """→ [(行号, 实参列表), ...]

    ★ 排除**函数声明** `func RequirePerm(st *store.Store, code permission.Code)`：
      那是守卫定义本身，不是受保护入口。
    """
    out = []
    for m in REQUIRE_PERM_RE.finditer(source):
        if re.search(r"\bfunc\s+$", source[:m.start()]):
            continue  # 声明，非调用
        start = m.end()
        depth, i, in_str, escape = 1, start, False, False
        while i < len(source):
            ch = source[i]
            if in_str:
                if escape:
                    escape = False
                elif ch == "\\":
                    escape = True
                elif ch == '"':
                    in_str = False
            elif ch == '"':
                in_str = True
            elif ch == "(":
                depth += 1
            elif ch == ")":
                depth -= 1
                if depth == 0:
                    break
            i += 1
        line = source.count("\n", 0, m.start()) + 1
        out.append((line, split_call_args(source[start:i])))
    return out


def load_env_file(root):
    """仓库根 .env 里未设置的变量并入 env（真实环境变量优先）。"""
    env = dict(os.environ)
    path = os.path.join(root, ".env")
    if not os.path.isfile(path):
        return env
    for raw in read(path).splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        k = k.strip()
        if k and k not in env:
            env[k] = v.strip().strip('"').strip("'")
    return env


# ---------------------------------------------------------------- 主判据

def check(root, db_check=True):
    """→ problem 列表（空 = 通过）。"""
    problems = []

    spec_path = os.path.join(root, "spec", "permission-points.json")
    if not os.path.isfile(spec_path):
        return ["[⓪] 找不到 %s" % spec_path]
    spec = json.loads(read(spec_path))
    codes = [p["code"] for p in spec["permission_points"]]
    roles = [r["code"] for r in spec["roles"]]
    seed = spec.get("seed", {})
    code_set = set(codes)

    sources = {}
    for path in _walk(root, ".go", include_tests=False):
        try:
            sources[path] = read(path)
        except Exception as exc:
            problems.append("[⓪] 读取 %s 失败: %s" % (path, exc))
    if not sources:
        return problems + ["[⓪] 没扫到任何 Go 源码（root=%s）" % root]

    # ---- 常量声明文件（③「单一文件」基线） ----
    decl_files = {p: s for p, s in sources.items() if DECL_RE.search(s)}
    if not decl_files:
        return problems + ['[③] 找不到权限点常量声明文件（形如 Xxx Code = "sys.perm.edit"）']
    if len(decl_files) > 1:
        problems.append(
            "[③] 权限点常量必须集中在**单一声明文件**，实际出现在 %d 个文件：%s"
            % (len(decl_files), ", ".join(os.path.relpath(p, root) for p in sorted(decl_files)))
        )

    name_to_value, value_to_names = {}, {}
    for path, src in decl_files.items():
        for name, value in DECL_RE.findall(src):
            name_to_value[name] = value
            value_to_names.setdefault(value, []).append(name)
            if not SPEC_CODE_RE.match(value):
                problems.append("[③] 常量 %s 的取值不像权限点 code：%r" % (name, value))

    # ---- ② 声明端 ----
    for value, names in sorted(value_to_names.items()):
        if value not in code_set:
            problems.append(
                "[②] 代码声明了 spec 未登记的权限点 %r（常量 %s）—— 没声明却用了"
                % (value, "/".join(names))
            )
    for code in codes:
        if code not in value_to_names:
            problems.append("[②] spec 登记的 %s 在 Go 代码里没有常量声明 —— 字典与代码脱节" % code)

    # ---- ③ 禁裸写 ----
    guard_refs = []  # (rel, line, ident)
    for path, src in sources.items():
        rel = os.path.relpath(path, root)
        is_decl = path in decl_files
        for line_no, args in find_require_perm_calls(src):
            if not args:
                problems.append("[③] %s:%d RequirePerm(...) 无实参" % (rel, line_no))
                continue
            string_args = [a for a in args if a.startswith('"')]
            if string_args:
                problems.append(
                    "[③] %s:%d 受保护入口裸写权限点字符串 %s —— 必须引用 permission 常量"
                    % (rel, line_no, " ".join(string_args))
                )
            idents = [a for a in args if IDENT_ARG_RE.fullmatch(a.strip())]
            if not idents:
                problems.append(
                    "[③] %s:%d RequirePerm 实参无法静态核验为 permission.常量（实参: %s）"
                    % (rel, line_no, "; ".join(args) or "空")
                )
            for ident in idents:
                guard_refs.append((rel, line_no, ident.split(".", 1)[1]))
        if not is_decl:
            for lit in STRING_LIT_RE.findall(src):
                if lit in code_set:
                    problems.append(
                        "[③] %s 出现权限点字符串字面量 %r —— 禁裸写，必须用 permission 常量"
                        % (rel, lit)
                    )
            for lit in CODE_CAST_RE.findall(src):
                problems.append(
                    "[③] %s 出现 Code(%r) 类型转换 —— 字符串→Code 只允许发生在声明文件内"
                    % (rel, lit)
                )

    for rel, line_no, ident in guard_refs:
        value = name_to_value.get(ident)
        if value is None:
            problems.append(
                "[②] %s:%d RequirePerm(permission.%s) 不是已声明的权限点常量" % (rel, line_no, ident)
            )
        elif value not in code_set:
            problems.append(
                "[②] %s:%d RequirePerm(permission.%s) → %r 未在 spec/permission-points.json 登记"
                % (rel, line_no, ident, value)
            )

    # 迁移 SQL：同样不许出现 code 字面量（字典必须由代码注册）
    for path in _walk(root, ".sql", include_tests=True):
        rel = os.path.relpath(path, root)
        try:
            sql = read(path)
        except Exception as exc:
            problems.append("[⓪] 读取 %s 失败: %s" % (path, exc))
            continue
        for lit in STRING_LIT_RE.findall(sql) + re.findall(r"'([^']*)'", sql):
            if lit in code_set:
                problems.append(
                    "[③] %s 出现权限点字符串字面量 %r —— 字典由代码注册，不得在 SQL 里硬编码"
                    % (rel, lit)
                )

    # ---- ① 消费端 ----
    outside = {p: s for p, s in sources.items() if p not in decl_files}
    for code in codes:
        names = value_to_names.get(code, [])
        consumed = any(
            re.compile(r"\b%s\b" % re.escape(n)).search(s)
            for n in names for s in outside.values()
        )
        if not consumed:
            problems.append(
                "[①] %s 没有任何代码消费（常量 %s 只出现在声明文件里）—— 配了没人用"
                % (code, "/".join(names) or "无")
            )

    # ---- ④ 种子一致（spec 侧） ----
    if set(seed.keys()) != code_set:
        problems.append(
            "[④] seed 键集合与权限点集合不一致（多出 %s / 缺失 %s）"
            % (sorted(set(seed) - code_set) or "无", sorted(code_set - set(seed)) or "无")
        )
    rows = 0
    incomplete = []
    for code in codes:
        m = seed.get(code) or {}
        if set(m.keys()) != set(roles):
            incomplete.append(code)
        rows += len(m)
    if incomplete:
        problems.append("[④] seed 未覆盖全部角色的权限点：%s" % ", ".join(incomplete))
    expect_rows = len(codes) * len(roles)
    if rows != expect_rows:
        problems.append("[④] seed 行数 %d ≠ 权限点×角色 = %d" % (rows, expect_rows))
    if len(codes) == 51 and len(roles) == 6 and rows != 306:
        problems.append("[④] ★ 固定口径 51 × 6 = 306，实际 seed 行数 %d" % rows)

    if db_check:
        problems.extend(check_db(root, len(codes), len(roles), rows))
    return problems


def check_db(root, env_codes, env_roles, env_rows):
    """④ 库内一侧：s_permission_point / s_role / s_role_permission 计数。"""
    env = load_env_file(root)
    if not env.get("JX_DB_DSN", "").strip():
        return ["[④] 缺 JX_DB_DSN（环境变量与仓库 .env 均未提供）—— 库内一致性无法校验，判红"]
    try:
        proc = subprocess.run(
            ["go", "run", "./scripts/jxq"],
            cwd=root, env=env, capture_output=True, text=True, timeout=180,
        )
    except Exception as exc:
        return ["[④] 执行 scripts/jxq 失败: %s" % exc]
    if proc.returncode != 0:
        return ["[④] 查库失败（数据库不可达或 SQL 出错）：%s"
                % (proc.stderr.strip() or proc.stdout.strip() or "无输出")]
    got = {}
    for line in proc.stdout.splitlines():
        if "=" in line:
            k, v = line.split("=", 1)
            try:
                got[k.strip()] = int(v.strip())
            except ValueError:
                pass
    out = []
    for key, want in (("points", env_codes), ("roles", env_roles), ("role_perm", env_rows)):
        if got.get(key) != want:
            out.append("[④] 库内 %s = %s，spec 侧应为 %d（不一致）"
                       % (key, got.get(key, "缺失"), want))
    return out


# ---------------------------------------------------------------- 自检（TC-M0-07 / TC-M0-08）

FIXTURE_SPEC = {
    "spec": "fixture",
    "permission_points": [
        {"code": "sys.demo", "module": "系统管理", "name": "演示点A", "levels": ["ALL", "NONE"]},
        {"code": "sys.other", "module": "系统管理", "name": "演示点B", "levels": ["ALL", "NONE"]},
    ],
    "roles": [
        {"code": "sysadmin", "name": "系统管理员", "kind": "system", "is_system": True},
    ],
    "seed": {
        "sys.demo": {"sysadmin": "ALL"},
        "sys.other": {"sysadmin": "ALL"},
    },
}

FIXTURE_DECL = (
    "package permission\n"
    "\n"
    "type Code string\n"
    "\n"
    "const (\n"
    "\tSysDemo  Code = \"sys.demo\"\n"
    "\tSysOther Code = \"sys.other\"\n"
    ")\n"
)

FIXTURE_ALL = "package permission\n\nvar All = []Code{SysDemo, SysOther}\n"

FIXTURE_ROUTES = (
    "package httpapi\n"
    "\n"
    "import \"github.com/chadhao/jx-lab-trace/internal/permission\"\n"
    "\n"
    "func routes(st interface{}) interface{} {\n"
    "\treturn RequirePerm(st, permission.SysDemo)\n"
    "}\n"
)


def write_fixture(root):
    write_file(os.path.join(root, "spec", "permission-points.json"),
               json.dumps(FIXTURE_SPEC, ensure_ascii=False, indent=2))
    write_file(os.path.join(root, "internal", "permission", "code.go"), FIXTURE_DECL)
    write_file(os.path.join(root, "internal", "permission", "all.go"), FIXTURE_ALL)
    write_file(os.path.join(root, "internal", "httpapi", "routes.go"), FIXTURE_ROUTES)


# 每个样本：改坏 fixture，并返回**期望命中的判据标签**
SELFTEST_CASES = {}


def selftest_case(name):
    def deco(fn):
        SELFTEST_CASES[name] = fn
        return fn
    return deco


@selftest_case("clean")
def _case_clean(_root):
    """干净样本必须零命中（否则报红无意义）。"""
    return []


@selftest_case("unused_point")
def _case_unused_point(root):
    """① 判据（TC-M0-08）：sys.other 从消费清单里删掉 ⇒ 报红。"""
    write_file(os.path.join(root, "internal", "permission", "all.go"),
               "package permission\n\nvar All = []Code{SysDemo}\n")
    return ["[①]"]


@selftest_case("undeclared_string")
def _case_undeclared_string(root):
    """②+③ 判据（TC-M0-07）：受保护入口裸写字符串 + 声明字典外标识 ⇒ 报红。"""
    write_file(
        os.path.join(root, "internal", "httpapi", "routes.go"),
        "package httpapi\n"
        "\n"
        "import \"github.com/chadhao/jx-lab-trace/internal/permission\"\n"
        "\n"
        "func routes(st interface{}) interface{} {\n"
        "\t_ = permission.SysDemo\n"
        "\treturn RequirePerm(st, \"sys.demo\")\n"
        "}\n",
    )
    write_file(
        os.path.join(root, "internal", "permission", "code.go"),
        "package permission\n"
        "\n"
        "type Code string\n"
        "\n"
        "const (\n"
        "\tSysDemo   Code = \"sys.demo\"\n"
        "\tSysOther  Code = \"sys.other\"\n"
        "\tHackPoint Code = \"hack.not.inspec\"\n"
        ")\n",
    )
    return ["[②]", "[③]"]


@selftest_case("seed_gap")
def _case_seed_gap(root):
    """④ 判据：seed 少一个权限点 ⇒ 报红。"""
    spec = json.loads(json.dumps(FIXTURE_SPEC))
    del spec["seed"]["sys.other"]
    write_file(os.path.join(root, "spec", "permission-points.json"),
               json.dumps(spec, ensure_ascii=False, indent=2))
    return ["[④]"]


def run_selftest(only=None):
    names = [n for n in SELFTEST_CASES if only in (None, "", n)]
    if not names:
        print("FAIL 未知自检样本：%s（可用：%s）" % (only, ", ".join(SELFTEST_CASES)))
        return 1
    failed = []
    for name in names:
        root = tempfile.mkdtemp(prefix="permreg_%s_" % name)
        try:
            write_fixture(root)
            expected_tags = SELFTEST_CASES[name](root)
            problems = check(root, db_check=False)
            ok = True
            details = []
            if name == "clean":
                ok = not problems
                details = problems[:5]
            else:
                got_tags = sorted({p.split("]")[0] + "]" for p in problems})
                for tag in expected_tags:
                    if not any(p.startswith(tag) for p in problems):
                        ok = False
                        details.append("缺 %s 类命中（实际：%s）" % (tag, ", ".join(got_tags) or "无"))
            print("  %-20s %s" % (name, "OK" if ok else "FAIL"))
            for d in details:
                print("      - %s" % d)
            if not ok:
                failed.append(name)
        finally:
            shutil.rmtree(root, ignore_errors=True)
    if failed:
        print("FAIL 自检未通过：%s" % ", ".join(failed))
        return 1
    print("OK 自检通过（%d 个样本的判据行为符合预期）" % len(names))
    return 0


# ---------------------------------------------------------------- 入口

def main(argv):
    only, selftest, i = None, False, 1
    while i < len(argv):
        a = argv[i]
        if a == "--selftest":
            selftest = True
        elif a == "--only":
            i += 1
            if i >= len(argv):
                print("FAIL --only 需要参数")
                return 1
            only = argv[i]
        else:
            print("FAIL 未知参数：%s" % a)
            return 1
        i += 1

    if selftest:
        return run_selftest(only)

    print("[权限点注册门禁] root=%s" % ROOT)
    problems = check(ROOT, db_check=True)
    spec = json.loads(read(os.path.join(ROOT, "spec", "permission-points.json")))
    if problems:
        print("FAIL 共 %d 项：" % len(problems))
        for p in problems:
            print("  " + p)
        return 1
    print("OK 权限点注册一致：spec %d 点 × %d 角色 = %d 行；①消费/②声明/③禁裸写/④种子 全部通过"
          % (len(spec["permission_points"]), len(spec["roles"]),
             len(spec["permission_points"]) * len(spec["roles"])))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
