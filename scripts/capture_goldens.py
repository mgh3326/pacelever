#!/usr/bin/env python3
"""Capture the installed scopefuel CLI, using only temporary synthetic state.

Run with a Python >= 3.11 interpreter. No provider is called, no operator state
is changed, and no catalog PUT occurs. Golden changes require review.
"""
import datetime as dt
import json
import os
import pathlib
import shutil
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
PROFILES = [
    ("sonnet", "high", ""), ("sonnet", "max", ""),
    ("opus", "low", ""), ("codex-sol", "max", ""),
    ("codex-sol", "xhigh", ""), ("codex-luna", "medium", ""),
    ("devin-swe2", "", ""), ("devin-swe2-medium", "", ""),
    ("pi-sonnet55", "high", ""), ("pi-haiku55", "medium", ""),
    ("cmd-dsflash41", "high", ""), ("codex-astra", "", "architect"),
    ("fable", "", ""), ("pi-opus55", "", ""),
]
POOLS = ["claude", "codex", "devin", "kimi", "grok", "kiro", "clinepass", "commandcode", "agy", "upstage", "omniroute"]
now = dt.datetime.now(dt.timezone.utc)

def obs(pool, used):
    return {"id": pool, "plan": "plus", "pool_class": "preserve" if pool == "codex" else "spend", "source": "fixture", "buckets": [
        {"label": w, "window": w, "used_pct": used, "resets_at": (now + dt.timedelta(hours=h)).isoformat(), "scope": {"kind": "account", "name": None}, "horizon": horizon}
        for w, h, horizon in [("5h", 3, "now"), ("7d", 144, "week"), ("30d", 600, "month")]] + ([{"label": r, "window": "", "used_pct": 0, "scope": {"kind": "model", "name": r}, "horizon": "now", "note": "Free tag observed"} for r in ["swe-2-high", "swe-2-medium", "swe-2-max"]] if pool == "devin" else [])}

def main():
    executable = shutil.which("scopefuel")
    if not executable:
        raise SystemExit("installed scopefuel required")
    cases = []
    with tempfile.TemporaryDirectory(prefix="pacelever-goldens-") as tmp:
        base = pathlib.Path(tmp)
        config = base / "config" / "scopefuel"
        config.mkdir(parents=True)
        cache = base / "cache.json"
        # A deliberately empty endpoint pair prevents stored credential fallback.
        env = dict(os.environ, XDG_CONFIG_HOME=str(base / "config"), XDG_DATA_HOME=str(base / "data"), SCOPEFUEL_CACHE=str(cache), SCOPEFUEL_STALE_WARN="0", SCOPEFUEL_QUOTA_SHARE="0", SCOPEFUEL_QUOTA_V2="0", HANDOFFKEEP_URL="https://example.invalid", HANDOFFKEEP_TOKEN="", HANDOFFKEEP_CONFIG=str(base / "absent.env"))
        env.pop("SCOPEFUEL_E6_ARM", None)
        config.joinpath("config.toml").write_text('[bench]\nbackend = "local"\n')
        cache.write_text(json.dumps({p: {"fetched_at": now.timestamp(), "result": obs(p, 10)} for p in POOLS}))
        for profile, effort, purpose in PROFILES:
            for role in ["", "worker", "tester", "builder"]:
                options = (["--effort", effort] if effort else []) + (["--role", role] if role else []) + (["--purpose", purpose] if purpose else [])
                launch = subprocess.run([executable, "policy", "launch", profile, *options, "--json"], env=env, capture_output=True, text=True, timeout=10)
                gate = subprocess.run([executable, "gate", "-m", profile, *options, "--cache-ttl", "3600"], env=env, capture_output=True, text=True, timeout=10)
                case = {"request": {"profile": profile, "effort": effort, "role": role, "purpose": purpose}, "launch_code": launch.returncode, "launch": json.loads(launch.stdout) if launch.returncode == 0 else None, "gate_code": gate.returncode}
                # PR 219 is deliberately newer than the installed reference.
                if profile in {"pi-sonnet55", "pi-haiku55", "cmd-dsflash41"} and role == "builder":
                    case["intentional_change"] = "placed default credit rung admits builder"
                cases.append(case)
        for state,used in [("exhausted",99),("unmeasurable",None),("unsubscribed",10)]:
            data = {p: {"fetched_at": now.timestamp(), "result": obs(p, used)} for p in POOLS}
            cache.write_text(json.dumps(data))
            config.joinpath("config.toml").write_text('[bench]\nbackend = "local"\n' + ('[pools.claude]\nsubscribed = false\n' if state == "unsubscribed" else ''))
            gate = subprocess.run([executable,"gate","-m","sonnet","--cache-ttl","3600"],env=env,capture_output=True,text=True,timeout=10)
            cases.append({"request":{"profile":"sonnet"},"state":state,"gate_code":gate.returncode})
    out = {"reference": "installed scopefuel 9fd1c45", "captured_at": now.isoformat(), "cases": cases}
    ROOT.joinpath("testdata/scopefuel_golden.json").write_text(json.dumps(out,indent=2,ensure_ascii=False)+"\n")
    print(f"Captured {len(cases)} cases; all fixtures stopped and temporary state removed")

if __name__ == "__main__":
    main()
