package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// windowTerminal opens and watches the terminal windows tincan window starts
// Claude Code in. cmux is the only one so far.
type windowTerminal interface {
	// open makes a new window in dir, without focusing it, with env
	// (KEY=VALUE) set for everything it runs, and returns its handle.
	open(ctx context.Context, name, dir string, env []string) (string, error)
	// screen returns the window's visible text. errWindowGone means the
	// window no longer exists.
	screen(ctx context.Context, handle string) (string, error)
	// typeLine types line into the window and presses Enter. line must be a
	// single line: the terminal treats a newline as Enter.
	typeLine(ctx context.Context, handle, line string) error
}

var errWindowGone = errors.New("the window is gone")

// defaultCmuxBin is where cmux installs its CLI. launchd's PATH does not
// include the app bundle, so the path is resolved before it is needed.
const defaultCmuxBin = "/Applications/cmux.app/Contents/Resources/bin/cmux"

// cmuxCallTimeout bounds one cmux CLI call, so a cmux that stopped answering
// cannot hold a worker forever.
const cmuxCallTimeout = 15 * time.Second

// How long typeLine waits for Claude Code's input box to clear.
const cmuxSendAttempts = 20

var cmuxSendRetry = 500 * time.Millisecond

// resolveCmux returns the cmux CLI to drive: flag, then CMUX_BIN, then the
// app bundle's, then PATH.
func resolveCmux(flag string) (string, error) {
	for _, p := range []string{flag, os.Getenv("CMUX_BIN")} {
		if p != "" {
			return exec.LookPath(expandHome(p))
		}
	}
	if _, err := os.Stat(defaultCmuxBin); err == nil {
		return defaultCmuxBin, nil
	}
	if p, err := exec.LookPath("cmux"); err == nil {
		return p, nil
	}
	return "", errors.New("cmux not found: install cmux from cmux.com, or pass --cmux with its CLI's path")
}

// cmuxTerminal drives cmux through its CLI, by workspace ref.
type cmuxTerminal struct{ bin string }

func (c cmuxTerminal) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cmuxCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, args...)
	// CMUX_QUIET silences the CLI's deprecation hints, which would otherwise
	// land in the output parsed here.
	cmd.Env = append(os.Environ(), "CMUX_QUIET=1")
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, cmuxError(err, text)
	}
	return text, nil
}

// cmuxError explains the failure a first-time setup hits: cmux's socket
// accepts only processes started inside cmux unless told otherwise, and a
// launchd service is not one.
func cmuxError(err error, out string) error {
	low := strings.ToLower(out)
	if strings.Contains(low, "access denied") || strings.Contains(low, "broken pipe") || strings.Contains(low, "only processes started inside cmux") {
		return fmt.Errorf("cmux refused the connection (%s): set cmux's automation.socketControlMode to allowAll or password and restart cmux; agentmail-to-claude-code's setup_cmux.py does it", out)
	}
	if out == "" {
		return fmt.Errorf("cmux: %w", err)
	}
	return fmt.Errorf("cmux: %s", out)
}

func (c cmuxTerminal) open(ctx context.Context, name, dir string, env []string) (string, error) {
	args := []string{"workspace", "create", "--name", name, "--cwd", dir, "--focus", "false"}
	for _, kv := range env {
		args = append(args, "--env", kv)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	// cmux prints "OK workspace:<n>".
	for f := range strings.FieldsSeq(out) {
		if strings.HasPrefix(f, "workspace:") {
			return f, nil
		}
	}
	return "", fmt.Errorf("cmux did not name the new workspace: %q", out)
}

func (c cmuxTerminal) screen(ctx context.Context, ws string) (string, error) {
	out, err := c.run(ctx, "read-screen", "--workspace", ws)
	if err != nil && strings.Contains(strings.ToLower(out), "not found") {
		return "", errWindowGone
	}
	return out, err
}

func (c cmuxTerminal) typeLine(ctx context.Context, ws, line string) error {
	if strings.ContainsAny(line, "\r\n") {
		return errors.New("a typed line cannot hold a newline: the terminal would press Enter early")
	}
	// cmux refuses to send while Claude Code's input box still holds text,
	// such as a message that has not been submitted yet, so wait for it to
	// clear rather than fail the request.
	for attempt := 1; ; attempt++ {
		out, err := c.run(ctx, "send", "--workspace", ws, line)
		if err == nil {
			break
		}
		if !strings.Contains(out, "holds text") || attempt == cmuxSendAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cmuxSendRetry):
		}
	}
	// Claude Code's input box takes a burst of text as a paste; a short
	// pause lets it settle before Enter submits it.
	time.Sleep(300 * time.Millisecond)
	_, err := c.run(ctx, "send-key", "--workspace", ws, "enter")
	return err
}

func newWindowTerminal(name, cmuxFlag string) (windowTerminal, error) {
	switch name {
	case "", "cmux":
		bin, err := resolveCmux(cmuxFlag)
		if err != nil {
			return nil, err
		}
		return cmuxTerminal{bin: bin}, nil
	}
	return nil, fmt.Errorf("--terminal %q is not supported: the only terminal so far is cmux", name)
}
