"""Run only the native binary shipped in this platform wheel."""
import os
from pathlib import Path
import subprocess
import sys


def main():
    binary = Path(__file__).with_name("agentfeedback.exe" if os.name == "nt" else "agentfeedback")
    arguments = [str(binary), *sys.argv[1:]]
    if os.name == "nt":
        child = subprocess.Popen(arguments)
        while True:
            try:
                return child.wait()
            except KeyboardInterrupt:
                # Console Ctrl-C also reaches the native child; let it own shutdown.
                continue
    os.execv(str(binary), arguments)
