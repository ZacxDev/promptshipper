#!/usr/bin/env python3
"""Pre-flight STT_API_TOKEN / STT_API_TOKENS BEFORE restarting the STT server.

The server parses both at import time and REFUSES BOOT on a malformed entry, so
a typo picked up by a restart takes every client down until it is fixed. This
runs app.py's OWN `parse_tokens` — sliced out of app.py's AST, so there is
exactly one copy of the rules — with the standard library only (no fastapi, no
model):

    set -a; . deploy/compose/.env; set +a
    python3 server/stt/check_tokens.py

Exit 0: prints the count and each entry's NAME and SCOPE. Exit 2: prints the
server's own refusal, which names the entry by POSITION and never its content.
No token is ever printed.
"""

import ast
import hashlib
import hmac
import os
import pathlib
import re
import sys

APP = pathlib.Path(__file__).resolve().with_name("app.py")
FUNCS = {"parse_tokens"}
CLASSES = {"TokenConfigError"}
CONSTS = {"LEGACY_TOKEN_NAME", "TOKEN_NAME_RE", "SCOPE_INTERNAL", "SCOPE_PUBLIC", "TOKEN_SCOPES"}


def load(app_path=APP):
    keep = []
    for node in ast.parse(pathlib.Path(app_path).read_text()).body:
        if isinstance(node, ast.FunctionDef) and node.name in FUNCS:
            keep.append(node)
        elif isinstance(node, ast.ClassDef) and node.name in CLASSES:
            keep.append(node)
        elif isinstance(node, ast.Assign) and {
            t.id for t in node.targets if isinstance(t, ast.Name)
        } & CONSTS:
            keep.append(node)
    keep.insert(0, ast.ImportFrom(module="__future__", names=[ast.alias("annotations")], level=0))
    mod = ast.fix_missing_locations(ast.Module(body=keep, type_ignores=[]))
    ns = {"re": re, "hashlib": hashlib, "hmac": hmac}
    exec(compile(mod, str(app_path), "exec"), ns)  # noqa: S102 - our own source
    missing = (FUNCS | CLASSES | CONSTS) - set(ns)
    if missing:
        raise SystemExit(f"check_tokens: {app_path} no longer defines {sorted(missing)}")
    return ns


def main(environ=os.environ, out=sys.stdout, err=sys.stderr) -> int:
    ns = load()
    try:
        table = ns["parse_tokens"](environ.get("STT_API_TOKEN", ""), environ.get("STT_API_TOKENS", ""))
    except ns["TokenConfigError"] as e:
        print(f"REFUSED — the STT server would not boot: {e}", file=err)
        return 2
    if not table:
        print("ok, but ZERO tokens: every authenticated route would answer 503", file=out)
        return 0
    print(f"ok: {len(table)} token(s)", file=out)
    for name, scope, _digest in table:
        print(f"  {name}  {scope}", file=out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
