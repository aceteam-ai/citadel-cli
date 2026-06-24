# Terminal "double-echo" (llss) — Root Cause Analysis

## Symptom
On Linux with **fish** as the shell, typing in the Citadel **TUI Console tab**
echoes each keystroke twice: `ls` -> `llss`, `docker-compose` -> `ddocker-compose`.

## Investigation summary (every echo surface checked)
This is NOT classic double-echo (two echo sources) and NOT a tmux problem.
Every byte-relay / renderer in the system echoes exactly ONCE (the PTY's ECHO):

| Surface | Input path | Echo | Verdict |
|---|---|---|---|
| Go Attach client (`internal/instance/client.go`) | raw stdin -> socket (`term.MakeRaw`) | PTY only | single, OK |
| Go instance server (`internal/instance/server.go`) | client -> PTY | PTY only | single, OK |
| Go WS terminal server (`internal/terminal/server.go`) | WS input -> PTY | PTY only | single, OK |
| Go TUI Console (`internal/tui/controlcenter/console_page.go`) | key -> PTY, consumes event | PTY only | single source, but see below |
| Web xterm.js (`aceteam/hooks/useWebTerminal.ts`, `components/fabric/WebTerminal.tsx`) | onData -> sendInput (server only); output -> write once | PTY only | single, OK |

The only `term.MakeRaw` in the whole repo is in the Attach client (correct).
No code path mirrors input back to output. So there is exactly ONE echo source
everywhere — the PTY — which rules out the "two echo sources" hypothesis.

## Actual root cause (fish-specific, TUI Console only)
The TUI Console does NOT use a terminal emulator. It renders PTY output into a
line-oriented `tview.TextView` after passing it through `consoleFilter`
(`internal/tui/controlcenter/console_filter.go`), which **strips carriage
returns** (added in #296 to avoid stray control glyphs).

fish redraws the *entire* command line on every keystroke (syntax highlighting +
autosuggestions). It does this in place using a carriage return (`\r`, return to
column 0) followed by the updated line. A real terminal emulator (web xterm.js,
raw-mode Attach client) honours the CR and **overwrites** the line, so you see a
single, correct prompt line.

The TUI Console **strips the CR**, so `tview.TextView` never returns to column 0;
each repaint chunk is **appended** instead of overwriting:

- type `l`: fish echoes `l` -> view shows `l`
- type `s`: fish repaints `\r` + `ls` -> CR stripped -> view appends `ls` -> `lls`...

producing the `llss` doubling. bash's default prompt does not repaint the whole
line on each keystroke, so it does not double the same way.

This is the same family as #296 (charset escapes leaking) and #307 (DA queries):
the line-oriented fake-terminal renderer choking on a control sequence that a
real terminal handles. It is a manifestation of the already-documented v1
limitation in `console_page.go`: "tview.TextView is line-oriented, not a terminal
emulator. Full-screen programs (vim, htop, etc.) will render incorrectly." fish's
interactive prompt is an in-place repainter and hits the same limit on the prompt.

## Fix direction
Do NOT disable PTY ECHO (regresses the TUI + Attach clients which depend on it,
and fish re-arms its own termios on every prompt). The fix must teach the
TUI Console renderer to honour in-place line repaints (CR -> column 0 overwrite,
cursor moves, erase-line) for the current line before committing it to the view,
while preserving #296's charset stripping. Multi-line / full-screen repaints
remain the pre-existing documented limitation.

Verification is non-PTY: the transform is a pure function table-tested against
captured fish output. Live TUI/PTY cannot be tested from the agent.
