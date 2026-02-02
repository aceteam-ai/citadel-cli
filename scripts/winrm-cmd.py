#!/usr/bin/env python3
"""Execute a PowerShell command on a remote Windows machine via WinRM.

Usage: python3 scripts/winrm-cmd.py <host> <user> <password> <powershell-command>

Uses HTTP transport (port 5985) with NTLM authentication.
Prints stdout to stdout, stderr to stderr. Exits non-zero on failure.
"""

import sys

if len(sys.argv) != 5:
    print(f"Usage: {sys.argv[0]} <host> <user> <password> <powershell-command>", file=sys.stderr)
    sys.exit(2)

host, user, password, command = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

try:
    import winrm
except ImportError:
    print("pywinrm not installed. Install with: pip install pywinrm", file=sys.stderr)
    sys.exit(2)

session = winrm.Session(
    f"http://{host}:5985/wsman",
    auth=(user, password),
    transport="ntlm",
)

result = session.run_ps(command)

if result.std_out:
    sys.stdout.buffer.write(result.std_out)
if result.std_err:
    sys.stderr.buffer.write(result.std_err)

sys.exit(result.status_code)
