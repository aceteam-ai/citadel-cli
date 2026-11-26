# Proxmox VM Interaction from CLI

Methods for interacting with Proxmox VMs remotely without the Proxmox web UI.

All machine names, accounts, addresses and VM IDs below are synthetic examples.
Substitute an operator-owned test environment before using any command.

## Prerequisites

```bash
# SSH access to Proxmox host
ssh root@hypervisor.example.invalid

# List VMs
qm list
```

## VM Reference

| VMID | Name | IP | SSH User | Purpose |
|------|------|----|----------|---------|
| 900 | example-linux-vm | — | — | Main dev VM |
| 901 | example-desktop-vm | desktop.example.invalid | operator | Windows 11 + RTX 2080 GPU passthrough |
| 902 | example-messaging-vm | — | — | WeChat (stopped) |

## 1. Screenshots (QEMU Monitor)

```bash
# Take screenshot (PPM format)
ssh root@hypervisor.example.invalid 'echo "screendump /tmp/screen.ppm" | qm monitor 901'

# Download and convert to PNG
scp root@hypervisor.example.invalid:/tmp/screen.ppm /tmp/screen.ppm
ffmpeg -y -i /tmp/screen.ppm /tmp/screen.png
```

## 2. Keyboard Input (QEMU Monitor)

```bash
# Send a single key
ssh root@hypervisor.example.invalid 'qm sendkey 901 ret'

# Common keys: ret, spc, tab, esc, backspace, delete
# Modifiers: ctrl, alt, shift (combine with -)
# Examples:
ssh root@hypervisor.example.invalid 'qm sendkey 901 ctrl-alt-delete'
ssh root@hypervisor.example.invalid 'qm sendkey 901 meta_l-r'          # Win+R (Run dialog)
ssh root@hypervisor.example.invalid 'qm sendkey 901 meta_l-e'          # Win+E (Explorer)

# Type text (one key at a time)
for c in c m d; do ssh root@hypervisor.example.invalid "qm sendkey 901 $c"; done
ssh root@hypervisor.example.invalid 'qm sendkey 901 ret'
```

## 3. Mouse Input (QEMU Monitor)

Requires the USB tablet device (absolute positioning). Check with:

```bash
ssh root@hypervisor.example.invalid 'echo "info mice" | qm monitor 901'
# Should show: * Mouse #N: QEMU HID Tablet (absolute)
```

```bash
# Move mouse to absolute position (screen coords, e.g. 1280x800)
ssh root@hypervisor.example.invalid 'printf "mouse_move 640 400\n" | qm monitor 901'

# Click (1=left, 2=middle, 4=right)
ssh root@hypervisor.example.invalid 'printf "mouse_button 1\nmouse_button 0\n" | qm monitor 901'

# Double-click (two clicks with short delay)
ssh root@hypervisor.example.invalid 'printf "mouse_move 200 200\nmouse_button 1\nmouse_button 0\n" | qm monitor 901'
sleep 0.2
ssh root@hypervisor.example.invalid 'printf "mouse_button 1\nmouse_button 0\n" | qm monitor 901'
```

## 4. Guest Agent Commands

The QEMU guest agent runs inside the VM. Commands execute in session 0 (no desktop access).

```bash
# Ping (check agent is alive)
ssh root@hypervisor.example.invalid 'qm guest cmd 901 ping'

# Run a command (stdout not returned by default)
ssh root@hypervisor.example.invalid 'qm guest exec 901 -- "C:\Users\operator\Downloads\citadel-new.exe" --version'

# Get network interfaces
ssh root@hypervisor.example.invalid 'qm guest cmd 901 network-get-interfaces'

# File operations
ssh root@hypervisor.example.invalid 'qm guest cmd 901 fsfreeze-status'
```

**Limitation:** Guest agent commands run in session 0 (SYSTEM context). They cannot start GUI/TUI applications on the interactive desktop. Use keyboard/mouse input or SSH for interactive work.

## 5. Power Management

```bash
ssh root@hypervisor.example.invalid 'qm status 901'      # Check status
ssh root@hypervisor.example.invalid 'qm start 901'       # Start VM
ssh root@hypervisor.example.invalid 'qm shutdown 901'    # Graceful shutdown (via guest agent)
ssh root@hypervisor.example.invalid 'qm stop 901'        # Force stop
ssh root@hypervisor.example.invalid 'qm reboot 901'      # Reboot
ssh root@hypervisor.example.invalid 'qm reset 901'       # Hard reset
```

## 6. Snapshots

```bash
ssh root@hypervisor.example.invalid 'qm listsnapshot 901'
ssh root@hypervisor.example.invalid 'qm snapshot 901 before-testing'
ssh root@hypervisor.example.invalid 'qm rollback 901 before-testing'
ssh root@hypervisor.example.invalid 'qm delsnapshot 901 before-testing'
```

## 7. Launch GUI App on Interactive Desktop (from SSH)

SSH runs in session 0 (services) on Windows — `Start-Process` and direct `exec` won't show on the desktop. Use `schtasks` with `/ru` to run as the logged-in user:

```bash
# Create and run an immediate task as the desktop user
ssh operator@desktop.example.invalid 'schtasks /create /tn "CitadelLaunch" /tr "cmd.exe /k C:\Users\operator\Downloads\citadel-new.exe" /sc once /st 00:00 /ru operator /f && schtasks /run /tn "CitadelLaunch" && timeout /t 2 && schtasks /delete /tn "CitadelLaunch" /f'
```

**Important:** Use `/ru <username>` to run as the desktop user, not SYSTEM. Without it, the task runs as SYSTEM (session 0) which has no desktop access and a different home directory.

## 8. Combined: Launch App via Win+R (QEMU sendkey)

When you need to start a console app on the interactive desktop via QEMU:

```bash
HOST=root@hypervisor.example.invalid
VMID=901

# Open Run dialog
ssh $HOST "qm sendkey $VMID meta_l-r"
sleep 1

# Type the command
CMD='cmd /k C:\Users\operator\Downloads\citadel-new.exe'
for char in $(echo "$CMD" | grep -o .); do
  case "$char" in
    ' ') key="spc" ;;
    '/') key="slash" ;;
    ':') key="shift-semicolon" ;;
    '\') key="backslash" ;;
    '.') key="dot" ;;
    '-') key="minus" ;;
    '_') key="shift-minus" ;;
    [A-Z]) key="shift-$(echo $char | tr '[:upper:]' '[:lower:]')" ;;
    *) key="$char" ;;
  esac
  ssh $HOST "qm sendkey $VMID $key"
done

# Press Enter
ssh $HOST "qm sendkey $VMID ret"

# Wait and screenshot
sleep 3
ssh $HOST "echo 'screendump /tmp/screen.ppm' | qm monitor $VMID"
scp $HOST:/tmp/screen.ppm /tmp/screen.ppm
ffmpeg -y -i /tmp/screen.ppm /tmp/screen.png
```

## Tips

- **Screen resolution**: Check with `ssh root@hypervisor.example.invalid 'echo "info vga" | qm monitor 901'` or take a screenshot and check dimensions
- **Mouse coordinates**: QEMU absolute mouse uses the VM's framebuffer resolution, not the Proxmox console resolution
- **Key names**: See QEMU docs for full key name list (`qemu-system-x86_64 -k help`), or use `qm sendkey` which accepts standard X11 key names
- **Timing**: Add `sleep` between keystrokes/clicks to account for Windows UI latency
