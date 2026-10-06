#!/usr/bin/env python3
"""Contract checker: api/openapi.yaml must exactly mirror the server routes.

Compares every mux route in internal/httpapi/server.go against the OpenAPI
contract (both directions) and verifies the document parses with all $ref
targets resolvable. Used by `go test ./scripts/` (contract_test) and in CI.

Exit codes: 0 = in sync, 1 = drift or invalid contract.
"""

import os
import re
import sys

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
YAML = os.path.join(ROOT, "api", "openapi.yaml")
SERVER = os.path.join(ROOT, "internal", "httpapi", "server.go")
API_PREFIX = "/api/v1"


def load_contract():
    """Returns (contract_ops, missing_refs, parse_ok)."""
    with open(YAML) as f:
        d = yaml.safe_load(f)
    if not isinstance(d, dict):
        raise ValueError("openapi.yaml did not parse to a mapping")
    ops = set()
    for path, method_defs in d.get("paths", {}).items():
        for method, op in method_defs.items():
            if method in ("get", "post", "patch", "put", "delete"):
                ops.add(f"{method.upper()} {path.replace(API_PREFIX, '')}")
    refs = {}
    def walk(o, refs):
        if isinstance(o, dict):
            for k, v in o.items():
                if k == "$ref" and isinstance(v, str):
                    seg = v.split("/")
                    if len(seg) >= 4:
                        refs.setdefault(seg[2], set()).add(seg[3])
                walk(v, refs)
        elif isinstance(o, list):
            for i in o:
                walk(i, refs)
    walk(d, refs)
    pool = {
        "schemas": d.get("components", {}).get("schemas", {}),
        "responses": d.get("components", {}).get("responses", {}),
        "parameters": d.get("components", {}).get("parameters", {}),
    }
    missing = [(sec, name) for sec, names in refs.items()
               for name in names if name not in pool.get(sec, {})]
    return ops, missing


def load_routes():
    """Returns every mux route in server.go, normalized (no API prefix)."""
    with open(SERVER) as f:
        src = f.read()
    routes = set()
    for method, path in re.findall(
        r'mux\.(?:Handle|HandleFunc)\("(GET|POST|PATCH|PUT|DELETE) ([^"]+)"', src
    ):
        routes.add(f"{method.upper()} {path.replace(API_PREFIX, '')}")
    return routes


def main() -> int:
    ops, missing = load_contract()
    if missing:
        for sec, name in missing:
            print(f"REF {sec}:{name} does not resolve")
        return 1
    routes = load_routes()
    drift = 0
    for op in sorted(routes - ops):
        print(f"CODE {op} missing from contract")
        drift += 1
    for op in sorted(ops - routes):
        print(f"CONTRACT {op} not implemented")
        drift += 1
    if drift == 0:
        print(f"contract in sync ({len(ops)} REST operations)")
        return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())