package main

import (
	"os"
	"runtime"
	"syscall"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/logging"
)

// qosClampEnv marks a process that has already been re-exec'd under
// taskpolicy, so the clamped process does not exec itself again. It is
// inherited by every child, including the evolve restart, which is what we
// want: the clamp is a task attribute that survives execve, so a child
// never needs to reapply it.
const qosClampEnv = "_RALPH_QOS_CLAMP"

const taskpolicyPath = "/usr/sbin/taskpolicy"

// execve is syscall.Exec, indirected so tests can observe the re-exec
// instead of having the test binary replaced.
var execve = syscall.Exec

// reexecUnderQoSClamp replaces the current process with
// `taskpolicy -c <clamp> <self> <args...>` so the loop and every process it
// spawns run under a macOS QoS clamp. The clamp is a spawn-time attribute
// (posix_spawnattr_set_qos_clamp_np) inherited by all children, which is why
// the loop applies it to itself rather than a shell wrapper applying it from
// outside: the loop pane is launched by the tmux server from os.Executable()'s
// absolute path and evolveRestart re-execs the same path, so no PATH wrapper
// ever sits in front of the process that matters.
//
// On success it never returns. It returns without exec'ing when not on
// darwin, when clamp is "none", or when the process is already clamped; a
// missing taskpolicy or a failed exec is logged once and the loop runs
// unclamped.
func reexecUnderQoSClamp(clamp string, args []string, log *logging.Logger) {
	if runtime.GOOS != "darwin" || clamp == config.QoSClampNone || os.Getenv(qosClampEnv) != "" {
		return
	}
	if _, err := os.Stat(taskpolicyPath); err != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "QoS clamp not applied: %s not found", taskpolicyPath)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "QoS clamp not applied: resolving executable: %v", err)
		return
	}
	argv := append([]string{"taskpolicy", "-c", clamp, exe}, args...)
	env := append(os.Environ(), qosClampEnv+"="+clamp)
	if err := execve(taskpolicyPath, argv, env); err != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "QoS clamp not applied: %v", err)
	}
}
