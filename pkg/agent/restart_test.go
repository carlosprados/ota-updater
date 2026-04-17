package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Test helper subprocess plumbing. Each Test* that needs to assert behavior
// of a syscall-level function (Exec, Exit) re-runs the test binary with one
// of these env vars set; TestMain dispatches into the helper before
// m.Run(), so the helper code lives at top level (no double init() needed).
const (
	execRestartHelperEnv = "OTA_TEST_EXEC_RESTART_TARGET"
	exitRestartHelperEnv = "OTA_TEST_EXIT_RESTART_CODE"
)

func TestMain(m *testing.M) {
	if target := os.Getenv(execRestartHelperEnv); target != "" {
		// Replace ourselves with target. Successful Exec never returns.
		err := ExecRestart{}.Restart(context.Background(), target, []string{filepath.Base(target)})
		fmt.Fprintf(os.Stderr, "ExecRestart returned: %v\n", err)
		os.Exit(99)
	}
	if codeStr := os.Getenv(exitRestartHelperEnv); codeStr != "" {
		code, err := strconv.Atoi(codeStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad %s value %q: %v\n", exitRestartHelperEnv, codeStr, err)
			os.Exit(98)
		}
		ExitRestart{Code: code}.Restart(context.Background(), "", nil)
		// Unreachable: ExitRestart.Restart calls os.Exit.
		os.Exit(97)
	}
	os.Exit(m.Run())
}

// recordRestart captures Restart invocations for tests that compose the
// strategy into something larger.
type recordRestart struct {
	calls  int
	binary string
	argv   []string
	err    error
}

func (r *recordRestart) Restart(_ context.Context, binary string, argv []string) error {
	r.calls++
	r.binary = binary
	r.argv = argv
	return r.err
}

func TestRecordRestart_RoundTrip(t *testing.T) {
	r := &recordRestart{}
	var s RestartStrategy = r
	if err := s.Restart(context.Background(), "/path/agent", []string{"agent", "-x"}); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if r.calls != 1 || r.binary != "/path/agent" || r.argv[1] != "-x" {
		t.Fatalf("unexpected capture: %+v", r)
	}
}

func TestExecRestart_RejectsEmptyBinary(t *testing.T) {
	err := ExecRestart{}.Restart(context.Background(), "", nil)
	if err == nil {
		t.Fatalf("empty binary should error")
	}
	if !strings.Contains(err.Error(), "binary path is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecRestart_ReturnsErrorOnNonexistentBinary(t *testing.T) {
	// A successful syscall.Exec never returns; the only way Restart returns
	// is on failure. Pointing at a definitely-missing path lets us assert
	// the failure path in-process without re-execing the test binary.
	err := ExecRestart{}.Restart(context.Background(), "/this/does/not/exist/binary", nil)
	if err == nil {
		t.Fatalf("expected error from exec of missing binary")
	}
	if !strings.Contains(err.Error(), "exec restart") {
		t.Fatalf("err missing prefix: %v", err)
	}
}

// TestExecRestart_ReplacesProcessImage spawns a child copy of the test binary
// with execRestartHelperEnv set to /bin/true. TestMain in the child intercepts
// that env var and asks ExecRestart to replace itself with /bin/true; if the
// exec succeeds the child exits 0 (true's exit). If anything in the helper
// returned to Go code, TestMain exits 99 instead — so a clean child exit
// proves syscall.Exec actually replaced the process image.
func TestExecRestart_ReplacesProcessImage(t *testing.T) {
	bt, err := exec.LookPath("/bin/true")
	if err != nil {
		t.Skipf("/bin/true not available: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestExecRestart_ReplacesProcessImage")
	cmd.Env = append(os.Environ(), execRestartHelperEnv+"="+bt)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("subprocess err = %v, output = %s", runErr, out)
	}
}

// TestExitRestart_NeverReturnsToCaller spawns a child copy of the test binary
// with exitRestartHelperEnv set to "42". The child's TestMain asks ExitRestart
// to exit with code 42; we verify the child terminated with that exact code.
func TestExitRestart_NeverReturnsToCaller(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestExitRestart_NeverReturnsToCaller")
	cmd.Env = append(os.Environ(), exitRestartHelperEnv+"=42")
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError from subprocess, got %v", err)
	}
	if exitErr.ExitCode() != 42 {
		t.Fatalf("subprocess exit code = %d, want 42", exitErr.ExitCode())
	}
}
