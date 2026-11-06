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
# Placeholder values that name no one: RFC 2606/6761 reserved domains for
# email, and an explicit list of generic user names in a path (a short name can
# be a real account, so length alone never qualifies). Fixtures use these so
# tests can exercise parsers without carrying real identities.
reserved_email = re.compile(r"@(?:[A-Za-z0-9-]+\.)*(?:example(?:\.(?:com|net|org))?|invalid|test|localhost)\Z", re.I)
placeholder_path = re.compile(r"/(?:Users|home)/(?:x|user|you|me|runner)/")
allowed = {
    "email": lambda m: reserved_email.search(m.group(0)) is not None,
    "personal filesystem path": lambda m: placeholder_path.fullmatch(m.group(0)) is not None,
}
failures = []
# -z: names arrive verbatim (no core.quotePath escaping of unusual bytes).
for name in subprocess.check_output(["git", "ls-files", "-z"], text=True).split("\0"):
    if not name:
        continue
    try:
        text = pathlib.Path(name).read_text()
    except (UnicodeDecodeError, FileNotFoundError):
        # Not text, or indexed but deleted from the working tree.
        continue
    for label, pattern in patterns.items():
        keep = allowed.get(label, lambda m: False)
        # Overlapping scan: a match's trailing '/' can start the next one
        # (/home/me/home/<name>/), and finditer would skip that occurrence.
        pos = 0
        while (m := pattern.search(text, pos)) is not None:
            if not keep(m):
                failures.append(f"{name}: {label}")
                break
            pos = m.start() + 1
if failures:
    raise SystemExit("\n".join(failures))
print("Public-content check passed (matched values are never printed)")
