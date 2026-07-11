// cmd/citadel-start/main.go
/*
Copyright © 2025 AceTeam <dev@aceteam.ai>

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

// Command citadel-start is a tiny double-click launcher for Windows.
//
// The Windows release zip ships two files: citadel.exe (the raw CLI) and this
// launcher. Double-clicking citadel.exe from Explorer flashes a console that
// prints --help and vanishes, which confused first-run users. citadel-start.exe
// is the obvious clickable "start the node" wrapper: it locates citadel.exe
// sitting next to it, runs "citadel.exe work", streams its output, and — the
// whole point — keeps the console window open on exit so the user actually sees
// what happened (including the "run citadel init first" error on a fresh node)
// instead of watching a window flash and disappear.
//
// The package is intentionally pure stdlib and OS-agnostic so it builds and its
// tests run on any platform (CI is linux); the release only ships the
// GOOS=windows binary.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// childName is the CLI binary the launcher execs, expected to sit in the same
// directory as the launcher itself.
const childName = "citadel.exe"

// startArg is the citadel subcommand that brings a node online.
const startArg = "work"

// noPauseEnv, when set to any non-empty value, disables the "Press Enter to
// close" pause. Useful for scripted/terminal launches and for tests.
const noPauseEnv = "CITADEL_START_NO_PAUSE"

// siblingPath returns the path to a file sitting next to the launcher
// executable, given the launcher's own resolved executable path. It never
// consults PATH or the current working directory, which are unreliable when the
// process is spawned by an Explorer double-click.
func siblingPath(selfExe, name string) string {
	return filepath.Join(filepath.Dir(selfExe), name)
}

// exitCodeFromErr maps the error returned by exec.Cmd.Run to a process exit
// code: nil -> 0, an *exec.ExitError -> the child's own code, and any other
// error (e.g. the binary could not be started) -> 1.
func exitCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// shouldPause reports whether the launcher should wait for a keypress before
// exiting. Pausing keeps the console window visible for a double-click user;
// setting noPauseEnv skips it.
func shouldPause(getenv func(string) string) bool {
	return getenv(noPauseEnv) == ""
}

// exitHint returns a human-readable next-step message for a non-zero child exit
// code, or "" when the child exited cleanly. The most common first-run failure
// is an uninitialized node, so the hint points the user at "citadel init".
func exitHint(code int) string {
	if code == 0 {
		return ""
	}
	return fmt.Sprintf("Citadel exited with code %d.\n"+
		"If this node has not been set up yet, open a terminal in this folder and run:\n"+
		"    %s init\n"+
		"then double-click this launcher again.", code, childName)
}

// run executes the launcher logic and returns the exit code to propagate. It is
// parameterized on its dependencies so the orchestration can be unit-tested
// without spawning a real process or touching os.Stdin/Stdout.
func run(
	selfExe string,
	statImpl func(string) (os.FileInfo, error),
	execImpl func(childPath string) int,
	out io.Writer,
) int {
	child := siblingPath(selfExe, childName)

	if _, err := statImpl(child); err != nil {
		fmt.Fprintf(out, "Cannot find %s next to this launcher.\n", childName)
		fmt.Fprintf(out, "Expected it at: %s\n", child)
		fmt.Fprintf(out, "Make sure you unzipped the whole archive and kept the files together.\n")
		return 2
	}

	fmt.Fprintf(out, "Starting Citadel node (%s %s)...\n\n", childName, startArg)
	code := execImpl(child)

	fmt.Fprintln(out)
	if hint := exitHint(code); hint != "" {
		fmt.Fprintln(out, hint)
	} else {
		fmt.Fprintln(out, "Citadel stopped.")
	}
	return code
}

// execChild runs "<childPath> work", wiring the child's stdio straight through
// to this process and setting its working directory to the launcher's own
// directory (mirroring the old citadel.bat's `cd /d "%~dp0"`). It returns the
// child's exit code.
func execChild(childPath string) int {
	cmd := exec.Command(childPath, startArg)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = filepath.Dir(childPath)
	return exitCodeFromErr(cmd.Run())
}

// pause blocks until the user presses Enter, keeping the console window open so
// a double-click user can read the output. It is a no-op when noPauseEnv is set.
func pause(getenv func(string) string, out io.Writer, in io.Reader) {
	if !shouldPause(getenv) {
		return
	}
	fmt.Fprint(out, "\nPress Enter to close this window...")
	_, _ = bufio.NewReader(in).ReadString('\n')
}

func main() {
	selfExe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not determine launcher location: %v\n", err)
		pause(os.Getenv, os.Stdout, os.Stdin)
		os.Exit(1)
	}

	code := run(selfExe, os.Stat, execChild, os.Stdout)
	pause(os.Getenv, os.Stdout, os.Stdin)
	os.Exit(code)
}
