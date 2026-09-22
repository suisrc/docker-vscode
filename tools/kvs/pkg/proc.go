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
	"unsafe"
)

// restoreForeground gives the controlling terminal's foreground process
// group back to the kvs process group (TIOCSPGRP). No-op when stdin is
// not a TTY (daemon / pipe / CI), or when the ioctl fails.
func restoreForeground() {
	pgid := syscall.Getpgrp()
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(syscall.Stdin),
		uintptr(syscall.TIOCSPGRP), uintptr(unsafe.Pointer(&pgid)))
	if errno == 0 {
		log.Printf("terminal foreground process group restored to %d", pgid)
	}
}

// terminateTimeout is how long terminateLocked waits after each signal
// (SIGTERM, then SIGKILL) before giving up and abandoning the child.
const terminateTimeout = 10 * time.Second

// supervisorHeadstart is a heuristic grace window between the wrapper's own
// SIGTERM and the forced termination of its process group. Expiry does NOT
// prove that respawn has stopped - it only means the wrapper was still
// alive (or its fate unknown) when the window closed, so the group is
// force-terminated either way. Instant-exiting wrappers do not wait it out:
// termination continues as soon as the wrapper is reaped or the group is
// empty.
const supervisorHeadstart = 2 * time.Second

// proc.go manages one kvs-managed backend subprocess (Process).
//
// Key invariants:
//   - Each child is launched in its own process group (Setpgid), so Stop
//     kills the whole backend tree, not just the top process.
//   - A reaper goroutine calls cmd.Wait (no zombie) and closes proc.done
//     once. done = "wrapper reaped" (liveness for Running()); group
//     liveness during termination is probed with kill(-pgid, 0), since
//     the wrapper usually dies before its children.
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
	// New session: detaches the backend from kvs's controlling terminal,
	// so it cannot tcsetpgrp-steal the foreground and swallow Ctrl+C.
	// Implies a fresh pgid, so -pgid signaling in Stop still works.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
		// Backend exit: if it had stolen the terminal foreground group via
		// tcsetpgrp, give it back to kvs so the next Ctrl+C reaches kvs.
		restoreForeground()
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

// terminateLocked terminates the current child and forgets it; callers
// hold e.lock for the whole escalation. The wrapper gets SIGTERM first
// (bare pid) so it stops respawning before its children die - signaling
// the group at once let it re-launch dying children, which was the
// "stop triggers a start" bug. Waiting is group-based (kill(-pgid, 0)),
// not done-based, since the wrapper usually dies first.
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
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		log.Printf("get pgid %d: %v, falling back to pid", pid, err)
		pgid = pid
	}
	// Step 1: SIGTERM the wrapper (bare pid) to stop its respawn logic
	// before its children start dying.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		log.Printf("kill [%s] pid %d: %v", e.Name, pid, err)
	} else {
		log.Printf("sent SIGTERM to [%s] main pid %d (supervisor first)", e.Name, pid)
	}
	// Step 2: bounded grace window, polled every 50ms, with the two exit
	// conditions checked in a fixed order. Empty group first: it is the
	// decisive state - nothing left to signal and no respawn possible,
	// whatever the wrapper's own fate (a childless backend reaches it in
	// one tick, and so does a wrapper that took its children along).
	// Then wrapper death: respawn has stopped, so fall through and clean
	// up whatever children are still in the group.
	wrapperDead := false
	deadline := time.Now().Add(supervisorHeadstart)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err != nil {
			log.Printf("[%s] process group %d exited gracefully", e.Name, pgid)
			e.proc = nil
			return
		}
		select {
		case <-e.proc.done:
			wrapperDead = true
		default:
		}
		if wrapperDead {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if wrapperDead {
		log.Printf("[%s] wrapper %d exited, cleaning up its group", e.Name, pid)
	}
	// Step 3: SIGTERM the remaining group members. ESRCH = already empty.
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err == nil {
		log.Printf("sent SIGTERM to [%s] process group %d", e.Name, pgid)
	}
	if !waitGroupEmpty(pgid, terminateTimeout) {
		// Step 4: SIGKILL the group. The wrapper is the group leader
		// (Setpgid), so -pgid reaches it as well and respawn cannot
		// survive - no separate bare-pid kill is needed, and after the
		// wrapper was reaped its pid may already be recycled, so
		// signaling it could hit an unrelated process. A group surviving
		// SIGKILL holds D-state members no signal can reach; the group is
		// abandoned (e.proc cleared) to keep the lock responsive - the
		// parent reaps them when it exits, so no zombie or pid leak.
		log.Printf("[%s] group %d did not exit after SIGTERM, sending SIGKILL", e.Name, pgid)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		if !waitGroupEmpty(pgid, terminateTimeout) {
			log.Printf("ERROR: [%s] group %d still alive %v after SIGKILL (D state?), abandoning it", e.Name, pgid, terminateTimeout)
		}
	}
	e.proc = nil
}

// waitGroupEmpty polls the group with signal 0 until empty (ESRCH) or
// timeout. Zombies count as present until reaped (the wrapper's by this
// reaper's cmd.Wait, orphaned children by init), which normally costs a
// few poll rounds; a zombie left unreaped long enough consumes the whole
// timeout, and the caller then escalates to SIGKILL - harmless for a
// zombie, but it does make the "did not exit after SIGTERM" path
// reachable without any real straggler.
func waitGroupEmpty(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(-pgid, 0); err != nil {
			return true // ESRCH: no process left in the group
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Running reports whether the tracked child is still alive (proc.done
// not closed) - true for tracked-but-crashed children is the bug this
// avoids by reading the channel rather than the proc pointer.
func (e *Process) Running() bool {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.aliveLocked()
}
