package mobile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"
)

// exitCoder is satisfied by *exec.ExitError and test fakes, allowing the runner
// to extract exit codes without depending on concrete exec types.
type exitCoder interface{ ExitCode() int }

// stepTimeout caps a single provisioning step. sdkmanager --licenses and
// keychain imports are quick; the cap guards against a hung interactive prompt.
const stepTimeout = 10 * time.Minute

// Runner executes a provisioning plan, or prints it in dry-run mode.
type Runner struct {
	// DryRun, when true, prints each step instead of executing it.
	DryRun bool
	// Out is the destination for human-readable progress output.
	Out io.Writer

	// execCommand is the exec hook, overridable in tests. When nil it defaults
	// to a real command runner.
	execCommand func(ctx context.Context, name string, args ...string) ([]byte, error)
	// copyFile is the copy hook, overridable in tests. When nil it defaults to
	// a real filesystem copy.
	copyFile func(src, dst string) error
}

// NewRunner returns a Runner writing progress to out. A nil out defaults to
// os.Stdout.
func NewRunner(dryRun bool, out io.Writer) *Runner {
	if out == nil {
		out = os.Stdout
	}
	return &Runner{DryRun: dryRun, Out: out}
}

// Run executes (or prints) every step in order. On the first failing step it
// stops and returns the error, leaving prior steps applied.
func (r *Runner) Run(steps []Step) error {
	for i, step := range steps {
		fmt.Fprintf(r.Out, "[%d/%d] %s\n", i+1, len(steps), step.Desc)
		if err := r.runStep(step); err != nil {
			return fmt.Errorf("step %d (%s): %w", i+1, step.Desc, err)
		}
	}
	return nil
}

func (r *Runner) runStep(step Step) error {
	switch step.Kind {
	case StepExec:
		return r.runExec(step)
	case StepCopyFile:
		return r.runCopy(step)
	default:
		return fmt.Errorf("unknown step kind %d", step.Kind)
	}
}

func (r *Runner) runExec(step Step) error {
	if r.DryRun {
		fmt.Fprintf(r.Out, "      $ %s\n", step.CommandString())
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	out, err := r.exec(ctx, step.Name, step.Args...)
	if len(out) > 0 {
		fmt.Fprintf(r.Out, "%s", out)
	}
	if err != nil {
		var ec exitCoder
		if errors.As(err, &ec) && len(step.AllowedExitCodes) > 0 && slices.Contains(step.AllowedExitCodes, ec.ExitCode()) {
			fmt.Fprintf(r.Out, "      (exit code %d tolerated)\n", ec.ExitCode())
			return nil
		}
		return fmt.Errorf("%s failed: %w", step.Name, err)
	}
	return nil
}

func (r *Runner) runCopy(step Step) error {
	if r.DryRun {
		fmt.Fprintf(r.Out, "      copy %s -> %s\n", step.SrcPath, step.DstPath)
		return nil
	}
	if err := r.copy(step.SrcPath, step.DstPath); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", step.SrcPath, step.DstPath, err)
	}
	return nil
}

func (r *Runner) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.execCommand != nil {
		return r.execCommand(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (r *Runner) copy(src, dst string) error {
	if r.copyFile != nil {
		return r.copyFile(src, dst)
	}
	return defaultCopyFile(src, dst)
}

// defaultCopyFile copies src to dst, creating dst's parent directory. It
// preserves nothing beyond the bytes; provisioning profiles are plain files.
func defaultCopyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
