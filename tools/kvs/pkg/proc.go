package pkg

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// terminateTimeout is how long terminateLocked waits after each signal
// (SIGTERM, then SIGKILL) before giving up and abandoning the child.
const terminateTimeout = 5 * time.Second

// proc.go manages one kvs-managed backend subprocess (Process).
//
// Key invariants:
//   - Each child is launched in its own process group (Setpgid), so Stop
//     kills the whole backend tree, not just the top process.
//   - A reaper goroutine calls cmd.Wait (no zombie) and closes proc.done
//     exactly once; that channel is the liveness signal for both
//     terminateLocked and Running().
//   - All state transitions hold e.lock.
//
// Command syntax: split with strings.Fields (no quoting, no arguments
// with spaces); leading VAR=value tokens are appended to the child env
// and override inherited ones.

// Process owns the lifecycle of one managed subprocess. Name is the log
// prefix (from the name argument of Start; "unknown-<pid>" fallback).
type Process struct {
	Name string
	// lock guards cmds, proc and Name.
	lock sync.Mutex
	// cmds is the command string of the last successful start, so
	// Restart("") can re-launch it (exec.Cmd is single-use).
	cmds string
	// proc is the currently tracked child. nil = never started or
	// cleared by Stop/Restart; NOT cleared when the child exits on its
	// own - Running() reads proc.done for liveness.
	proc *struct {
		cmd  *exec.Cmd
		done chan struct{} // closed by the reaper once cmd.Wait returned
	}
}

// Start launches cmds (optionally prefixed with VAR=value assignments)
// in its own process group, with a reaper goroutine calling cmd.Wait.
// Fails if a child is still running; a dead child (crashed) is replaced.
func (e *Process) Start(name, cmds string) error {
	e.lock.Lock()
	defer e.lock.Unlock()
	// Replacing a still-alive child would orphan it; a dead child is
	// fine (reaper already reaped it).
	if e.aliveLocked() {
		return errors.New("process is running")
	}
	return e.startLocked(name, cmds)
}

// aliveLocked reports whether the tracked child is still alive
// (proc.done not closed). Callers must hold e.lock.
func (e *Process) aliveLocked() bool {
	if e.proc == nil {
		return false
	}
	select {
	case <-e.proc.done: // already exited and reaped
		return false
	default:
		return true
	}
}

// startLocked launches cmds and wires up tracking. Callers must hold
// e.lock and have terminated any previous child first.
func (e *Process) startLocked(name, cmds string) error {
	parts := strings.Fields(cmds)
	if len(parts) == 0 {
		return errors.New("command is empty")
	}
	// Pull leading VAR=value tokens into the child env. A token counts
	// as an assignment only when the part before "=" looks like a
	// variable name (non-empty, no "/" or space), so paths like
	// "./bin" still resolve to the command itself.
	env := os.Environ()
	i := 0
	for i < len(parts) {
		if k, _, ok := strings.Cut(parts[i], "="); ok && k != "" && !strings.ContainsAny(k, "/ ") {
			env = append(env, parts[i])
			i++
			continue
		}
		break
	}
	if i >= len(parts) {
		return errors.New("only env assignments")
	}
	cmd := exec.Command(parts[i], parts[i+1:]...)
	if i > 0 {
		cmd.Env = env // nil Env would inherit the parent env untouched
	}
	// Inherit stdout/stderr so backend logs interleave with kvs logs.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group, so signals can be sent to -pgid (whole tree).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Printf("[%s] start process: %v", name, err)
		return err
	}
	if name != "" {
		e.Name = name
	}
	if e.Name == "" {
		e.Name = "unknown-" + strconv.Itoa(cmd.Process.Pid)
	}
	log.Printf("[%s] process started (pid %d)", e.Name, cmd.Process.Pid)

	proc := &struct {
		cmd  *exec.Cmd
		done chan struct{}
	}{cmd: cmd, done: make(chan struct{})}
	// Capture the label so the reaper never reads e.Name (data race).
	label := e.Name
	go func() {
		defer close(proc.done)
		if err := cmd.Wait(); err != nil {
			log.Printf("[%s] process (pid %d) exited: %v", label, cmd.Process.Pid, err)
		} else {
			log.Printf("[%s] process (pid %d) exited", label, cmd.Process.Pid)
		}
	}()
	e.proc = proc
	e.cmds = cmds
	return nil
}

// Restart replaces the current child. With cmds == "" the command
// remembered by the last successful Start is re-launched (exec.Cmd is
// single-use, so the child is rebuilt from the string; this also works
// after a crash). Returns true when the replacement child is running.
func (e *Process) Restart(cmds string) bool {
	e.lock.Lock()
	defer e.lock.Unlock()
	if cmds == "" {
		cmds = e.cmds // re-launch the remembered command
		if cmds == "" {
			log.Printf("[%s] restart process: no previous command", e.Name)
			return false
		}
	}
	e.terminateLocked()
	if err := e.startLocked("", cmds); err != nil { // "" keeps the label
		log.Printf("[%s] restart process: %v", e.Name, err)
		return false
	}
	return true
}

// Stop terminates the tracked subprocess, if any, and forgets it
// (e.proc = nil), so a subsequent Start may launch a fresh child.
func (e *Process) Stop() {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.terminateLocked()
}

// terminateLocked terminates the current child (SIGTERM to the process
// group, then SIGKILL after terminateTimeout) and forgets it. Callers
// must hold e.lock, which stays held for the full escalation, so
// concurrent Running()/Start() calls block until the child is gone.
// An already-dead child is dropped without signaling; one that survives
// even SIGKILL (D state) is abandoned after terminateTimeout to keep the
// lock responsive - its reaper still collects it later (no zombie).
func (e *Process) terminateLocked() {
	if e.proc == nil || e.proc.cmd == nil || e.proc.cmd.Process == nil {
		return
	}
	pid := e.proc.cmd.Process.Pid
	// Already exited and reaped (e.g. crashed on its own): just forget it.
	select {
	case <-e.proc.done:
		e.proc = nil
		return
	default:
	}
	// Children are always launched with Setpgid, so pgid == pid; on a
	// reap race fall back to the bare pid so a live child is still signaled.
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		log.Printf("get pgid %d: %v, falling back to pid", pid, err)
		pgid = pid
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		log.Printf("kill [%s] pgid %d: %v", e.Name, pgid, err)
	} else {
		log.Printf("sent SIGTERM to [%s] process group %d", e.Name, pgid)
	}

	log.Printf("[%s] process %d stopping", e.Name, pid)
	select {
	case <-e.proc.done:
		log.Printf("[%s] process %d exited cleanly", e.Name, pid)
	case <-time.After(terminateTimeout):
		log.Printf("[%s] process %d did not exit after SIGTERM, sending SIGKILL", e.Name, pid)
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			log.Printf("kill -9 [%s] pgid %d: %v", e.Name, pgid, err)
		}
		// Bound the post-SIGKILL wait: a D-state child survives SIGKILL,
		// and waiting forever would wedge e.lock (and all its callers).
		select {
		case <-e.proc.done:
			log.Printf("[%s] process %d exited after SIGKILL", e.Name, pid)
		case <-time.After(terminateTimeout):
			log.Printf("ERROR: [%s] process %d still alive %v after SIGKILL, abandoning it", e.Name, pid, terminateTimeout)
		}
	}
	e.proc = nil
}

// Running reports whether the tracked child is still alive (proc.done
// not closed) - true for tracked-but-crashed children is the bug this
// avoids by reading the channel rather than the proc pointer.
func (e *Process) Running() bool {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.aliveLocked()
}
