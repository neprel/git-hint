#!/usr/bin/env python3
"""Generates the synthetic episode corpus (OpenCode 2.0 export form) and its expected
labels. The expected labels are written by hand below, before any extraction rule exists;
they are frozen once committed. Run: python3 gen.py (writes dev/ and heldout/)."""
import json, os, shutil

T0 = 1792000000000
MIN = 60_000


class S:
    """One session builder. Snapshot names (s0, s1, ...) refer to file trees of the case."""
    def __init__(self, sid, directory="/work/shop", parent=None):
        self.sid, self.dir, self.parent, self.msgs, self.n = sid, directory, parent, [], 0

    def user(self, rid, text, t):
        self.msgs.append({"type": "user", "id": rid, "text": text, "time": {"created": T0 + t * MIN}})
        return self

    def step(self, rid, t, start, end, tools=(), text="", finish="tool-calls"):
        content = []
        if text:
            content.append({"type": "text", "text": text})
        for i, (name, inp, status) in enumerate(tools):
            md = {"sessionId": inp.pop("_child")} if "_child" in inp else {}
            content.append({"type": "tool", "id": f"{rid}_t{i}", "name": name,
                            "state": {"status": status, "input": inp, "content": [], "metadata": md},
                            "time": {"created": T0 + t * MIN + i * 1000, "completed": T0 + t * MIN + i * 1000 + 500}})
        self.msgs.append({"type": "assistant", "id": rid, "agent": "build", "finish": finish, "cost": 0,
                          "model": {"id": "local-coder", "providerID": "lan", "variant": "default"},
                          "time": {"created": T0 + t * MIN, "completed": T0 + t * MIN + 30000},
                          "tokens": {"input": 400, "output": 40, "reasoning": 0, "cache": {"read": 0, "write": 0}},
                          "snapshot": {"start": "SNAP:" + start, "end": "SNAP:" + end}, "content": content})
        return self

    def doc(self):
        return {"info": {"id": self.sid, "projectID": "prj_corpus", "title": "synthetic", "agent": "build", "cost": 0,
                         "location": {"directory": self.dir}, "model": {"id": "local-coder", "providerID": "lan", "variant": "default"},
                         "time": {"created": T0, "updated": T0 + 999 * MIN},
                         "tokens": {"input": 0, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}}},
                "messages": self.msgs, "_parent": self.parent}


def read(p): return ("read", {"filePath": "/work/shop/" + p}, "completed")
def edit(p): return ("edit", {"filePath": "/work/shop/" + p, "oldString": "a", "newString": "b"}, "completed")
def test(ok=True): return ("bash", {"command": "go test ./..."}, "completed" if ok else "error")
def child(cid): return ("task", {"description": "help", "prompt": "check", "subagent_type": "general", "_child": cid}, "completed")


BASE = {"cart.go": "package shop\n\nfunc Total(p, d int) int { return p }\n", "cart_test.go": "package shop\n",
        "user.go": "package shop\n\nfunc Name() string { return \"\" }\n", "go.mod": "module shop\n"}


def tree(**changes):
    t = dict(BASE)
    for k, v in changes.items():
        k = k.replace("__", ".").replace("_go", ".go")
        if v is None:
            t.pop(k, None)
        else:
            t[k] = v
    return t


CASES = {}


def case(split, name, snapshots, sessions, expected, now=T0 + 600 * MIN, note=""):
    CASES[(split, name)] = {"note": note, "now": now, "snapshots": snapshots,
                            "sessions": [s.doc() for s in sessions], "expected": expected}


FIX = "package shop\n\nfunc Total(p, d int) int { return p - d }\n"
FIXR = "package shop\n\nfunc Total(p, d int) int { return round(p - d) }\n"
NAME = "package shop\n\nfunc Name() string { return \"guest\" }\n"

# ---------------------------------------------------------------- dev split
case("dev", "single-bugfix", {"s0": tree(), "s1": tree(cart_go=FIX)},
     [S("ses_a").user("u1", "Total ignores the discount; fix the bug.", 0)
      .step("a1", 1, "s0", "s1", [read("cart.go"), edit("cart.go"), test(True)])
      .step("a2", 2, "s1", "s1", text="Fixed; tests pass.", finish="stop")],
     [{"session": "ses_a", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "bug_fix", "links": [], "reasons": []}],
     note="one request, one edit, passing test")

case("dev", "correction", {"s0": tree(), "s1": tree(cart_go=FIX), "s2": tree(cart_go=FIXR)},
     [S("ses_b").user("u1", "Fix the discount bug in Total.", 0)
      .step("a1", 1, "s0", "s1", [edit("cart.go")], finish="stop")
      .user("u2", "That is wrong: it must also round the result.", 3)
      .step("a2", 4, "s1", "s2", [edit("cart.go"), test(True)], finish="stop")],
     [{"session": "ses_b", "requests": ["u1"], "corrections": ["u2"], "base": "s0", "result": "s2", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "bug_fix", "links": [], "reasons": []}],
     note="a correction before any passing check stays in the episode")

case("dev", "two-tasks-one-session", {"s0": tree(), "s1": tree(cart_go=FIX), "s2": tree(cart_go=FIX, user_go=NAME)},
     [S("ses_c").user("u1", "Fix the discount bug in Total.", 0)
      .step("a1", 1, "s0", "s1", [edit("cart.go"), test(True)], finish="stop")
      .user("u2", "Now add a default guest name to Name.", 10)
      .step("a2", 11, "s1", "s2", [edit("user.go"), test(True)], finish="stop")],
     [{"session": "ses_c", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "bug_fix", "links": [], "reasons": []},
      {"session": "ses_c", "requests": ["u2"], "corrections": [], "base": "s1", "result": "s2", "paths": ["user.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "feature", "links": [], "reasons": []}],
     note="a new request after a passing check with disjoint files is a new task")

case("dev", "no-check", {"s0": tree(), "s1": tree(user_go=NAME)},
     [S("ses_d").user("u1", "Add a default guest name.", 0)
      .step("a1", 1, "s0", "s1", [edit("user.go")], text="Done.", finish="stop")],
     [{"session": "ses_d", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["user.go"],
       "attribution": "exclusive", "outcome": "none", "family": "feature", "links": [], "reasons": ["no_checkable_outcome"]}],
     note="only the agent's own claim; no executable outcome")

case("dev", "missing-baseline", {"s1": tree(cart_go=FIX)},
     [S("ses_e").user("u1", "Fix the discount bug.", 0)
      .step("a1", 1, "gone", "s1", [edit("cart.go"), test(True)], finish="stop")],
     [{"session": "ses_e", "requests": ["u1"], "corrections": [], "base": None, "result": "s1", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "bug_fix", "links": [], "reasons": ["baseline_missing"]}],
     note="the pre-edit snapshot no longer exists: base unavailable, never current HEAD")

case("dev", "parallel-sessions-overlap", {"s0": tree(), "s1": tree(cart_go=FIX), "s2": tree(cart_go=FIXR)},
     [S("ses_f1").user("u1", "Fix the discount bug.", 0).step("a1", 2, "s0", "s1", [edit("cart.go"), test(True)], finish="stop"),
      S("ses_f2").user("v1", "Round the total.", 1).step("b1", 3, "s0", "s2", [edit("cart.go"), test(True)], finish="stop")],
     [{"session": "ses_f1", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["cart.go"],
       "attribution": "ambiguous", "outcome": "test_passed", "family": "bug_fix", "links": [], "reasons": ["attribution_ambiguous"]},
      {"session": "ses_f2", "requests": ["v1"], "corrections": [], "base": "s0", "result": "s2", "paths": ["cart.go"],
       "attribution": "ambiguous", "outcome": "test_passed", "family": None, "links": [], "reasons": ["attribution_ambiguous"]}],
     note="two sessions edit the same file in overlapping time: ownership stays ambiguous")

case("dev", "child-agent", {"s0": tree(), "s1": tree(cart_go=FIX)},
     [S("ses_g").user("u1", "Fix the discount bug in Total.", 0)
      .step("a1", 1, "s0", "s1", [child("ses_g_child"), edit("cart.go"), test(True)], finish="stop"),
      S("ses_g_child", parent="ses_g").user("c1", "check", 1).step("ca1", 1, "s0", "s0", [read("cart.go")], finish="stop")],
     [{"session": "ses_g", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "bug_fix", "links": [], "reasons": [], "child_sessions": 1}],
     note="child work is part of the parent episode; the child prompt is not a task")

case("dev", "failed-outcome", {"s0": tree(), "s1": tree(cart_go=FIX)},
     [S("ses_h").user("u1", "Fix the discount bug in Total.", 0)
      .step("a1", 1, "s0", "s1", [edit("cart.go"), test(False)], text="Tests still fail.", finish="stop")],
     [{"session": "ses_h", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_failed", "family": "bug_fix", "links": [], "reasons": []}],
     note="a failed attempt is kept as evidence")

case("dev", "open-episode", {"s0": tree(), "s1": tree(cart_go=FIX)},
     [S("ses_i").user("u1", "Fix the discount bug in Total.", 0)
      .step("a1", 1, "s0", "s1", [edit("cart.go")], finish="tool-calls")],
     [], now=T0 + 10 * MIN,
     note="activity 9 minutes ago: the episode is still open and is not proposed")

# ---------------------------------------------------------------- held-out split
case("heldout", "refactor-request", {"s0": tree(), "s1": tree(cart_go="package shop\n\nfunc Total(price, discount int) int { return price }\n")},
     [S("ses_k").user("u1", "Refactor Total: rename the parameters to price and discount.", 0)
      .step("a1", 1, "s0", "s1", [edit("cart.go"), test(True)], finish="stop")],
     [{"session": "ses_k", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "refactor", "links": [], "reasons": []}])

case("heldout", "api-change", {"s0": tree(), "s1": tree(cart_go="package shop\n\nfunc Total(p, d, tax int) int { return p - d + tax }\n")},
     [S("ses_l").user("u1", "Change the Total API signature to accept a tax argument; keep callers compatible.", 0)
      .step("a1", 1, "s0", "s1", [edit("cart.go"), test(True)], finish="stop")],
     [{"session": "ses_l", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "compatibility", "links": [], "reasons": []}])

case("heldout", "three-requests", {"s0": tree(), "s1": tree(cart_go=FIX), "s2": tree(cart_go=FIXR), "s3": tree(cart_go=FIXR, user_go=NAME)},
     [S("ses_m").user("u1", "Fix the discount bug.", 0)
      .step("a1", 1, "s0", "s1", [edit("cart.go")], finish="stop")
      .user("u2", "No, the bug is still there: round it.", 2)
      .step("a2", 3, "s1", "s2", [edit("cart.go"), test(True)], finish="stop")
      .user("u3", "Add a guest default name.", 20)
      .step("a3", 21, "s2", "s3", [edit("user.go"), test(True)], finish="stop")],
     [{"session": "ses_m", "requests": ["u1"], "corrections": ["u2"], "base": "s0", "result": "s2", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "bug_fix", "links": [], "reasons": []},
      {"session": "ses_m", "requests": ["u3"], "corrections": [], "base": "s2", "result": "s3", "paths": ["user.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "feature", "links": [], "reasons": []}])

case("heldout", "same-file-next-task", {"s0": tree(), "s1": tree(cart_go=FIX), "s2": tree(cart_go=FIXR)},
     [S("ses_n").user("u1", "Fix the discount bug.", 0)
      .step("a1", 1, "s0", "s1", [edit("cart.go"), test(True)], finish="stop")
      .user("u2", "Now add rounding to Total.", 15)
      .step("a2", 16, "s1", "s2", [edit("cart.go"), test(True)], finish="stop")],
     [{"session": "ses_n", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "bug_fix", "links": [], "reasons": []},
      {"session": "ses_n", "requests": ["u2"], "corrections": [], "base": "s1", "result": "s2", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": "feature", "links": [{"kind": "depends_on", "on_request": "u1", "status": "inferred"}], "reasons": []}],
     note="a passing check closes the first task; the next request on the same file depends on it")

case("heldout", "cross-session-continuation", {"s0": tree(), "s1": tree(cart_go=FIX), "s2": tree(cart_go=FIXR)},
     [S("ses_o1").user("u1", "Fix the discount bug.", 0).step("a1", 1, "s0", "s1", [edit("cart.go")], finish="stop"),
      S("ses_o2").user("v1", "Continue: the discount fix still needs rounding.", 120).step("b1", 121, "s1", "s2", [edit("cart.go"), test(True)], finish="stop")],
     [{"session": "ses_o1", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "none", "family": "bug_fix", "links": [], "reasons": ["no_checkable_outcome"]},
      {"session": "ses_o2", "requests": ["v1"], "corrections": [], "base": "s1", "result": "s2", "paths": ["cart.go"],
       "attribution": "exclusive", "outcome": "test_passed", "family": None, "links": [{"kind": "continues", "on_request": "u1", "status": "inferred"}], "reasons": []}],
     note="a later session continues unfinished work on the same file: linked, not merged")

case("heldout", "parallel-disjoint", {"s0": tree(), "s1": tree(cart_go=FIX), "s2": tree(user_go=NAME)},
     [S("ses_p1").user("u1", "Fix the discount bug.", 0).step("a1", 2, "s0", "s1", [edit("cart.go"), test(True)], finish="stop"),
      S("ses_p2").user("v1", "Add a guest default name.", 1).step("b1", 3, "s0", "s2", [edit("user.go"), test(True)], finish="stop")],
     [{"session": "ses_p1", "requests": ["u1"], "corrections": [], "base": "s0", "result": "s1", "paths": ["cart.go"],
       "attribution": "separated", "outcome": "test_passed", "family": "bug_fix", "links": [], "reasons": []},
      {"session": "ses_p2", "requests": ["v1"], "corrections": [], "base": "s0", "result": "s2", "paths": ["user.go"],
       "attribution": "separated", "outcome": "test_passed", "family": "feature", "links": [], "reasons": []}],
     note="overlapping time, disjoint files: separated with evidence")

case("heldout", "edits-without-request", {"s0": tree(), "s1": tree(cart_go=FIX)},
     [S("ses_q", parent="ses_x").user("c1", "do it", 0).step("a1", 1, "s0", "s1", [edit("cart.go"), test(True)], finish="stop")],
     [], note="a child session alone has no user request: no episode")


def write():
    for split in ("dev", "heldout"):
        shutil.rmtree(split, ignore_errors=True)
    for (split, name), c in sorted(CASES.items()):
        d = os.path.join(split, name)
        os.makedirs(d)
        exp = c.pop("expected")
        json.dump(c, open(os.path.join(d, "case.json"), "w"), indent=1, sort_keys=True)
        json.dump({"episodes": exp}, open(os.path.join(d, "expected.json"), "w"), indent=1, sort_keys=True)


if __name__ == "__main__":
    write()
    print(len(CASES), "cases")
