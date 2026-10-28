#!/usr/bin/env python3
"""Check tracked public artifacts without echoing matched values."""
import pathlib
import re
import subprocess

patterns = {
    "email": re.compile(r"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b"),
    "private address": re.compile(r"\b(?:10\.(?:\d{1,3}\.){2}\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})\b"),
    "credential": re.compile(r"(?:gh[pousr]_[A-Za-z0-9]{20,}|sk-ant-[A-Za-z0-9_-]{20,}|-----BEGIN (?:RSA |OPENSSH )?PRIVATE KEY-----)"),
    "personal filesystem path": re.compile(r"/(?:Users|home)/[A-Za-z0-9_.-]+/"),
}
failures = []
for name in subprocess.check_output(["git", "ls-files"], text=True).splitlines():
    try:
        text = pathlib.Path(name).read_text()
    except UnicodeDecodeError:
        continue
    for label, pattern in patterns.items():
        if pattern.search(text):
            failures.append(f"{name}: {label}")
if failures:
    raise SystemExit("\n".join(failures))
print("Public-content check passed (matched values are never printed)")
