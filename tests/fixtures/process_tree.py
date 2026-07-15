"""Fixture process: spawn a sleeping descendant and signal readiness."""

from __future__ import annotations

import subprocess
import sys
import time
from pathlib import Path


ready = Path(sys.argv[1])
child = subprocess.Popen(
    [sys.executable, "-c", "import time; time.sleep(120)"],
    stdin=subprocess.DEVNULL,
    stdout=subprocess.DEVNULL,
    stderr=subprocess.DEVNULL,
)
ready.write_text(str(child.pid), encoding="utf-8")
print("parent-ready", flush=True)
time.sleep(120)

