package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// RestartStrategy decides how the agent re-launches itself after a successful
// A/B swap. The contract is asymmetric: a successful Restart never returns
// (the strategy is expected to either replace the process image or terminate
// it). A returning Restart call therefore always means failure.
//
// The default ExecRestart uses syscall.Exec to keep PID, cgroup, env vars
// and inherited file descriptors stable — transparent to systemd and Docker.
// See README.md "Self-restart after swap" for the compatibility matrix and
// when to prefer ExitRestart instead.
type RestartStrategy interface {
	// Restart replaces or terminates the current process so the binary at
	// path takes over. argv[0] is binary by convention. ctx exists so
	// implementations can honor cancellation while preparing (e.g. flushing
	// state); once the actual exec/exit happens, ctx is moot.
	Restart(ctx context.Context, binary string, argv []string) error
}

// ExecRestart replaces the current process image via syscall.Exec. PID,
// cgroup, controlling terminal, environment variables, working directory and
// open file descriptors (without FD_CLOEXEC) are preserved. This is the
// default and the recommended strategy for both bare-metal+systemd and
// containerized deployments.
type ExecRestart struct {
	// Env is the environment passed to the new image. When nil, os.Environ()
	// is used (the common case; preserves NOTIFY_SOCKET for systemd notify
	// services and any caller-injected variables).
	Env []string
}

// Restart implements RestartStrategy. It only returns on failure: a
// successful syscall.Exec replaces the running process and never returns
// to Go code.
func (e ExecRestart) Restart(_ context.Context, binary string, argv []string) error {
	if binary == "" {
		return errors.New("exec restart: binary path is required")
	}
	if len(argv) == 0 {
		argv = []string{binary}
	}
	env := e.Env
	if env == nil {
		env = os.Environ()
	}
	if err := syscall.Exec(binary, argv, env); err != nil {
		return fmt.Errorf("exec restart: %w", err)
	}
	// Unreachable: a successful Exec replaces this process image.
	return nil
}

// ExitRestart terminates the current process with Code, expecting an external
// supervisor (systemd Restart=always, docker --restart=always, runit, etc.)
// to relaunch the binary. Useful when:
//
//   - the host enforces a "fresh process per start" policy,
//   - you want OTA-driven restarts to count against systemd's StartLimitBurst,
//   - the agent runs as a library inside a binary that already owns the
//     restart path and just wants the OTA layer to signal "time to exit".
type ExitRestart struct {
	// Code is the exit status. Zero is treated as success by most supervisors;
	// callers that want supervisors to flag the restart as a fault can set a
	// non-zero value.
	Code int
}

// Restart implements RestartStrategy. It calls os.Exit and therefore never
// returns; the error in the signature is for interface compliance only.
func (e ExitRestart) Restart(_ context.Context, _ string, _ []string) error {
	os.Exit(e.Code)
	return nil // unreachable
}
