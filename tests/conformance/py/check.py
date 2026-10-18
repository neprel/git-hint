#!/usr/bin/env python3
"""Independent check of the HINT v0.1 conformance fixtures (no HINT code).

It uses a different JSON Schema validator (Python `jsonschema`, MIT) and a different
RFC 8785 implementation (`rfc8785`, Apache-2.0) than the Go reference checker.
It checks: schema validity of all valid documents; schema rejection of every
schema-level invalid fixture; schema acceptance of every semantic-level fixture;
content digests and snapshot digests; identifier and digest test vectors.
The semantic rules beyond digests are specified in prose (hint-data §9.2) and are
checked by the Go reference checker.

Requirements: jsonschema==4.23.0, rfc8785==0.1.4
Usage (from the repository root): python3 -I tests/conformance/py/check.py
"""
import glob
import hashlib
import hmac
import json
import os
import sys

try:
    import rfc8785
    from jsonschema import Draft202012Validator
    from referencing import Registry, Resource
except ImportError as e:
    sys.exit(f"missing dependency: {e.name}; install jsonschema==4.23.0 and rfc8785==0.1.4 (see README)")

ROOT = sys.argv[1] if len(sys.argv) > 1 else "."
CONF = os.path.join(ROOT, "tests", "conformance")


def load(p):
    with open(p) as f:
        return json.load(f)


def registry():
    reg = Registry()
    for p in glob.glob(os.path.join(ROOT, "schemas", "v0.*", "*.schema.json")):
        s = load(p)
        reg = reg.with_resource(s["$id"], Resource.from_contents(s))
    return reg


def digest(v):
    return "sha256:" + hashlib.sha256(rfc8785.dumps(v)).hexdigest()


def snapshot_digest(payload, sid):
    snap = {k: v for k, v in next(s for s in payload["snapshots"] if s["snapshot_id"] == sid).items() if k != "digest"}
    pick = lambda key: [r for r in payload.get(key, []) if r["snapshot_id"] == sid]
    return digest({"snapshot": snap, "entities": pick("entities"), "relations": pick("relations"),
                   "constraints": pick("constraints")})


def main():
    reg = registry()
    validators = {v: Draft202012Validator({"$ref": f"urn:hint:schema:v{v}:envelope"}, registry=reg) for v in ("0.1", "0.2")}

    class ByVersion:
        """Validates a document with the schema set of its hint_version (v0.2 = v0.1 + context)."""
        def pick(self, d):
            v = ".".join(str(d.get("hint_version", "")).split(".")[:2])
            return validators.get(v)

        def iter_errors(self, d):
            val = self.pick(d)
            return iter([type("E", (), {"message": "unsupported hint_version"})()]) if val is None else val.iter_errors(d)

        def is_valid(self, d):
            val = self.pick(d)
            return val is not None and val.is_valid(d)
    validator = ByVersion()
    errors = []
    valid = sorted(glob.glob(os.path.join(ROOT, "examples", "v0.1", "*.json")) +
                   glob.glob(os.path.join(ROOT, "examples", "v0.2", "*.json")) +
                   glob.glob(os.path.join(CONF, "valid", "*.json")) +
                   glob.glob(os.path.join(CONF, "resolve", "cases", "*.json")))
    n_valid = 0
    for p in valid:
        d = load(p)
        name = os.path.relpath(p, ROOT)
        if "resolve" in p and d.get("hint_version") == "0.2.0":
            continue  # the version case is rejected by design
        errs = list(validator.iter_errors(d))
        if errs:
            errors.append(f"{name}: schema: {errs[0].message[:120]}")
            continue
        n_valid += 1
        if "tampered" in p:
            if digest(d["payload"]) == d["content_digest"]:
                errors.append(f"{name}: tampered document has a matching digest")
            continue
        if digest(d["payload"]) != d["content_digest"]:
            errors.append(f"{name}: content digest")
        if d["document_type"] == "repository_snapshot":
            for s in d["payload"]["snapshots"]:
                if snapshot_digest(d["payload"], s["snapshot_id"]) != s["digest"]:
                    errors.append(f"{name}: snapshot digest")
    expected = load(os.path.join(CONF, "invalid", "expected.json"))
    n_schema = n_sem = 0
    for f, e in sorted(expected.items()):
        d = load(os.path.join(CONF, "invalid", f))
        ok = validator.is_valid(d)
        if e["level"] == "schema":
            n_schema += 1
            if ok:
                errors.append(f"invalid/{f}: schema accepted it")
        else:
            n_sem += 1
            if not ok:
                errors.append(f"invalid/{f}: must be schema-valid")
            if e["code"] == "digest_mismatch" and digest(d["payload"]) == d["content_digest"]:
                errors.append(f"invalid/{f}: digest matches")
            if e["code"] == "snapshot_digest_mismatch":
                s = d["payload"]["snapshots"][0]
                if snapshot_digest(d["payload"], s["snapshot_id"]) == s["digest"]:
                    errors.append(f"invalid/{f}: snapshot digest matches")
    ids = load(os.path.join(CONF, "vectors", "ids-v0.1.json"))
    keys = {k: bytes.fromhex(v) for k, v in ids["keys"].items()}
    for v in ids["vectors"]:
        msg = ("hint:v0.1:" + v["domain"]).encode() + b"".join(b"\x00" + p.encode() for p in v["parts"])
        got = hmac.new(keys[v["key"]], msg, hashlib.sha256).hexdigest()[:2 * v.get("bytes", 16)]
        if got != v["id"]:
            errors.append(f"id vector {v['domain']} {v['parts']}: {got}")
    for v in load(os.path.join(CONF, "vectors", "digests-v0.1.json"))["vectors"]:
        if rfc8785.dumps(v["value"]).decode() != v["canonical"] or digest(v["value"]) != v["digest"]:
            errors.append(f"digest vector {v['canonical']}")
    for e in errors:
        print("FAIL", e)
    print(f"valid documents: {n_valid}; schema-invalid fixtures: {n_schema}; semantic fixtures: {n_sem}; "
          f"id vectors: {len(ids['vectors'])}; errors: {len(errors)}")
    sys.exit(1 if errors else 0)


if __name__ == "__main__":
    main()
