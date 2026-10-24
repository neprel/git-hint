# The host side of SSHCapture: one read-only docker inspect of one container, filtered on the
# host to the capture allowlist before anything is printed. It never reads the environment or
# prints the command; only the six allowlisted flags leave the host. Run as
# `python3 -I - <container>` with this file on stdin.
import json, re, socket, subprocess, sys

NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}")
VALUE = re.compile(r"[A-Za-z0-9_.:/-]{1,128}")
BOOLS = {"prompt_tokens_details": "enable-prompt-tokens-details", "prefix_caching": "enable-prefix-caching"}
VALUES = {"served_model": "served-model-name", "reasoning_parser": "reasoning-parser", "tool_call_parser": "tool-call-parser"}
INVALID = "\x00invalid"  # never matches VALUE: the client maps it to an unusable value


def fail(code):
    print(json.dumps({"error": code}))
    sys.exit(3)


def flags(cmd):
    # every occurrence of every --flag: a value (--flag value or --flag=value) or True (bare)
    seen = {}
    i = 0
    while i < len(cmd):
        a = cmd[i] if isinstance(cmd[i], str) else ""
        if a.startswith("--"):
            k, eq, v = a[2:].partition("=")
            if eq:
                seen.setdefault(k, []).append(v)
            elif i + 1 < len(cmd) and isinstance(cmd[i + 1], str) and not cmd[i + 1].startswith("--"):
                seen.setdefault(k, []).append(cmd[i + 1])
                i += 1
            else:
                seen.setdefault(k, []).append(True)
        i += 1
    return seen


def boolean(seen, f):
    # exactly one bare --enable-X (True) or one bare --no-enable-X (False); anything else
    # (absent, repeated, both forms, a value) is unknown, never inferred
    on, off = seen.get(f, []), seen.get("no-" + f, [])
    if on == [True] and not off:
        return True
    if off == [True] and not on:
        return False
    return None


def value(seen, k):
    # exactly one value matching VALUE; repeated or malformed is the invalid marker
    v = seen.get(k)
    if v is None:
        return None
    return v[0] if len(v) == 1 and isinstance(v[0], str) and VALUE.fullmatch(v[0]) else INVALID


def main():
    if len(sys.argv) != 2 or not NAME.fullmatch(sys.argv[1]):
        fail("unreadable")
    try:
        p = subprocess.run(["docker", "inspect", "--type", "container", "--", sys.argv[1]],
                           stdin=subprocess.DEVNULL, capture_output=True, timeout=20)
    except subprocess.TimeoutExpired:
        fail("inspect_timeout")
    except OSError:
        fail("inspect_failed")
    if p.returncode != 0:
        fail("inspect_failed")
    try:
        docs = json.loads(p.stdout)
    except ValueError:
        fail("unreadable")
    if not isinstance(docs, list) or len(docs) != 1 or not isinstance(docs[0], dict):
        fail("not_one_container")
    c = docs[0]
    state = c.get("State") if isinstance(c.get("State"), dict) else {}
    cfg = c.get("Config") if isinstance(c.get("Config"), dict) else {}
    cmd = cfg.get("Cmd") if isinstance(cfg.get("Cmd"), list) else []
    seen = flags(cmd)
    out = {"image_id": c.get("Image") if isinstance(c.get("Image"), str) else "",
           "host": socket.gethostname(),
           "container": c.get("Id") if isinstance(c.get("Id"), str) else "",
           "started_at": state.get("StartedAt") if isinstance(state.get("StartedAt"), str) else "",
           "restart_count": c.get("RestartCount") if type(c.get("RestartCount")) is int else None}
    for k, f in BOOLS.items():
        out[k] = boolean(seen, f)
    m = value(seen, "max-model-len")
    out["max_model_len"] = None if m is None else int(m) if m.isascii() and m.isdigit() and len(m) <= 10 else -1
    for k, f in VALUES.items():
        out[k] = value(seen, f)
    for k in ("image_id", "host", "container", "started_at"):
        if len(out[k]) > 128:
            out[k] = ""
    line = json.dumps(out, sort_keys=True)
    if len(line) > 4096:
        fail("unreadable")
    print(line)


main()
