// Package main implements kas: a tiny PID-1 process manager for containers.
//
// kas combines the responsibilities of tini (reap zombies, forward signals to
// the process group) with a configuration-driven manager that reads an ini file
// using the well-known [program:name] section syntax and adds `type`, `depends`,
// `max_retries` and `restart_delay` directives.
//
// Configuration file default location: /etc/kas.ini
// The config is read on startup and re-read on `kas reload` or SIGHUP (no
// polling); the managed process set is then reconciled with the new config.
//
// `autostart` doubles as runtime control: flipping it to `false` in the config
// stops the program; flipping back to `true` starts it again.
//
// Build: go build -o kas .
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
)

const (
	defaultConfigPath = "/etc/kas.ini"
	defaultStopWait   = 10 * time.Second
	restartInitDelay  = 500 * time.Millisecond
	restartMaxDelay   = 30 * time.Second
	// defaultMaxRetries caps auto-restart attempts when max_retries is unset. 0
	// would mean "retry forever" which risks a crash loop, so we default to 3.
	defaultMaxRetries = 3
	// onceDoneDir holds marker files for completed "once" programs. A marker
	// file named after the program survives kas process restarts and container
	// restarts (since /var/run is tmpfs, preserved across restarts). It is
	// cleared only when the container is recreated, so a "once" task re-runs
	// on recreate but not on restart.
	onceDoneDir = "/var/run/kas/once.done"
	// defaultSockPath is the unix socket used for IPC between kas and `kas ps`.
	defaultSockPath = "/var/run/kas/kas.sock"
)

// programConfig is the parsed configuration for a single [program:name] section.
type programConfig struct {
	Name      string
	Command   string
	Autostart bool
	// Autorestart controls whether a crashed program is restarted.
	Autorestart  bool
	StopWaitSecs time.Duration
	User         string
	Environment  []string
	StdoutLog    string
	StderrLog    string
	Priority     int
	// Type is "long" (default, a long-running service) or "once" (a one-shot
	// init task: started only on the first config load, never restarted).
	Type string
	// Deps is the list of program names that must be started (and, for "once"
	// deps, completed) before this program starts.
	Deps []string
	// MaxRetries caps the number of auto-restart attempts after a crash. 0 means
	// retry forever. Once the cap is hit the program is left stopped until a
	// config reload resets the counter.
	MaxRetries int
	// RestartDelay is the fixed delay between restart attempts. When 0, kas uses
	// exponential backoff (restartInitDelay doubling up to restartMaxDelay).
	RestartDelay time.Duration
	// Shell is the shell used to execute Command. Defaults to "false" (direct
	// exec without a shell). Set to "/bin/sh", "/bin/bash", etc. to run the
	// command through that shell (enables &&, |, >, $VAR, etc.).
	Shell string
}

// runningProc tracks a live process.
type runningProc struct {
	cmd       *exec.Cmd
	done      chan struct{}      // closed when the process has been reaped
	waitCh    chan processStatus // reaper delivers the exit status here
	stopMu    sync.Mutex
	stopped   bool // true when intentionally stopped, suppress autorestart
	startedAt time.Time
	restarts  int    // number of auto-restart attempts made
	status    string // "starting", "running", "stopped"
}

// processStatus is the reaped exit status of a child.
type processStatus struct {
	pid    int
	exit   int
	signal bool
}

// kas is the kas core: it owns the configured programs and their live processes.
type kas struct {
	mu     sync.Mutex
	progs  map[string]*programConfig
	procs  map[string]*runningProc
	logger *log.Logger

	// ranOnce records which "once" programs have already completed. Backed by
	// marker files under onceDoneDir so the state survives kas restarts.
	// Guarded by mu.
	ranOnce map[string]bool

	// depCh is signalled (non-blocking) when a "once" program finishes, so the
	// main loop can re-reconcile and start programs that depend on it.
	depCh chan struct{}

	// reaper state: maps a child pid to its runningProc so the single reaper
	// goroutine can deliver exit statuses. All access guarded by reapMu.
	reapMu    sync.Mutex
	reapTable map[int]*runningProc
}

func newKas() *kas {
	k := &kas{
		progs:     make(map[string]*programConfig),
		procs:     make(map[string]*runningProc),
		ranOnce:   loadRanOnce(),
		depCh:     make(chan struct{}, 1),
		logger:    log.New(os.Stdout, "[kas] ", log.LstdFlags|log.Lmsgprefix),
		reapTable: make(map[int]*runningProc),
	}
	go k.reaper()
	return k
}

// loadRanOnce scans onceDoneDir and returns the set of completed "once" programs.
func loadRanOnce() map[string]bool {
	m := make(map[string]bool)
	entries, err := os.ReadDir(onceDoneDir)
	if err != nil {
		return m
	}
	for _, e := range entries {
		if !e.IsDir() {
			m[e.Name()] = true
		}
	}
	return m
}

// markRanOnce persists a marker file so the "once" program is not re-run after
// a kas restart.
func markRanOnce(name string) {
	if err := os.MkdirAll(onceDoneDir, 0o755); err != nil {
		return
	}
	marker := filepath.Join(onceDoneDir, name)
	_ = os.WriteFile(marker, []byte("done\n"), 0o644)
}

// reaperRegister associates a child pid with its runningProc.
func (s *kas) reaperRegister(pid int, rp *runningProc) {
	s.reapMu.Lock()
	s.reapTable[pid] = rp
	s.reapMu.Unlock()
}

// reaperUnregister removes a pid mapping when supervise synthesizes an exit
// (process vanished before the reaper delivered a status), preventing the
// reaper from blocking on a waitCh with no receiver.
func (s *kas) reaperUnregister(pid int) {
	s.reapMu.Lock()
	delete(s.reapTable, pid)
	s.reapMu.Unlock()
}

// reaper is the single goroutine that calls Wait4(-1) to reap ALL children
// (both managed programs and orphans reparented to PID 1). Managed processes
// are looked up in reapTable and their status delivered via rp.waitCh; unknown
// pids (orphans) are simply reaped to prevent zombies. This avoids the race
// where Wait4(-1) in one goroutine steals the exit status that cmd.Wait() in
// another goroutine was waiting for.
func (s *kas) reaper() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil) // blocking until a child exits
		if pid <= 0 || err != nil {
			// ECHILD (no children) or error: back off to avoid busy-spin.
			time.Sleep(200 * time.Millisecond)
			continue
		}
		s.reapMu.Lock()
		rp, ok := s.reapTable[pid]
		if ok {
			delete(s.reapTable, pid)
		}
		s.reapMu.Unlock()

		st := processStatus{pid: pid}
		if ws.Exited() {
			st.exit = ws.ExitStatus()
		} else if ws.Signaled() {
			st.exit = 128 + int(ws.Signal())
			st.signal = true
		}
		if ok {
			// Blocking send: waitCh has buffer 1 and supervise always
			// eventually receives. This guarantees the exit status is never
			// lost even if supervise hasn't started receiving yet.
			rp.waitCh <- st
		}
		// Unknown pids (orphans) are now reaped; nothing else to do.
	}
}

// ---- ini parsing (hand-rolled, no third-party deps) -----------------------

// parseConfig parses a kas ini file ( [program:name] sections ) into a map of configs.
func parseConfig(path string) (map[string]*programConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	progs := make(map[string]*programConfig)
	var cur *programConfig

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip inline comments (';' / '#' outside quotes).
		if idx := indexComment(line); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(line[1 : len(line)-1])
			if !strings.HasPrefix(section, "program:") {
				// Ignore non-program sections (global options, etc.).
				cur = nil
				continue
			}
			name := strings.TrimSpace(strings.TrimPrefix(section, "program:"))
			if name == "" {
				return nil, fmt.Errorf("line %d: empty program name", lineNo)
			}
			cur = &programConfig{
				Name:         name,
				Autostart:    true,
				Autorestart:  true,
				StopWaitSecs: defaultStopWait,
				Priority:     999,
				Type:         "long",
				MaxRetries:   defaultMaxRetries,
				Shell:        "false",
			}
			if _, dup := progs[name]; dup {
				return nil, fmt.Errorf("line %d: duplicate program %q", lineNo, name)
			}
			progs[name] = cur
			continue
		}
		if cur == nil {
			// Key outside any program section: ignore (global options).
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected key=value, got %q", lineNo, raw)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if err := applyKV(cur, key, val); err != nil {
			return nil, fmt.Errorf("line %d: %v", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return progs, nil
}

// indexComment returns the index of an unquoted ';' or '#' comment marker, or -1.
func indexComment(s string) int {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case ';', '#':
			if !inSingle && !inDouble {
				return i
			}
		}
	}
	return -1
}

func applyKV(p *programConfig, key, val string) error {
	// Expand ${VAR} references from the kas process environment.
	val = os.Expand(val, os.Getenv)
	switch strings.ToLower(key) {
	case "command":
		if val == "" {
			return errors.New("command must not be empty")
		}
		p.Command = val
	case "autostart":
		p.Autostart = parseBool(val, true)
	case "autorestart":
		// Accepts true/false; the value "unexpected" is treated as true for compatibility.
		p.Autorestart = parseBool(val, true)
	case "stopwaitsecs":
		p.StopWaitSecs = parseSeconds(val, defaultStopWait)
	case "max_retries":
		// kas extension: cap on auto-restart attempts; 0 = retry forever.
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil || n < 0 {
			return fmt.Errorf("invalid max_retries %q (want non-negative integer)", val)
		}
		p.MaxRetries = n
	case "restart_delay":
		// kas extension: fixed seconds between restart attempts. 0 = exponential
		// backoff (default).
		p.RestartDelay = parseSeconds(val, 0)
	case "user":
		p.User = val
	case "environment":
		p.Environment = parseEnvironment(val)
	case "stdout_logfile":
		p.StdoutLog = val
	case "stderr_logfile":
		p.StderrLog = val
	case "priority":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("invalid priority %q", val)
		}
		p.Priority = n
	case "type":
		// kas extension: once (one-shot init) or long (default, service).
		v := strings.ToLower(strings.TrimSpace(val))
		switch v {
		case "once", "long":
			p.Type = v
		default:
			return fmt.Errorf("invalid type %q (want once|long)", val)
		}
		if p.Type == "once" {
			// once tasks are never auto-restarted.
			p.Autorestart = false
		}
	case "depends":
		// kas extension: comma-separated list of program names to start first.
		for _, d := range strings.Split(val, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				p.Deps = append(p.Deps, d)
			}
		}
	case "shell":
		// kas extension: shell to execute command with (default /bin/sh).
		// Set to "false" to exec the command directly without a shell.
		p.Shell = val
	default:
		// Unknown keys are ignored for forward compatibility.
	}
	return nil
}

func parseBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "1", "on":
		return true
	case "false", "no", "0", "off":
		return false
	default:
		return def
	}
}

func parseSeconds(v string, def time.Duration) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return def
	}
	return time.Duration(n) * time.Second
}

// parseEnvironment parses a "KEY=val,KEY2=val2" string. Surrounding quotes on
// the value are stripped to match Supervisor semantics.
func parseEnvironment(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	var out []string
	var sb strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			sb.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			sb.WriteByte(c)
		case c == ',' && !inSingle && !inDouble:
			if s := strings.TrimSpace(sb.String()); s != "" {
				out = append(out, stripValueQuotes(s))
			}
			sb.Reset()
		default:
			sb.WriteByte(c)
		}
	}
	if s := strings.TrimSpace(sb.String()); s != "" {
		out = append(out, stripValueQuotes(s))
	}
	return out
}

// stripValueQuotes strips one surrounding pair of single or double quotes from
// the value part of "KEY=VAL" (not the key).
func stripValueQuotes(s string) string {
	eq := strings.IndexByte(s, '=')
	if eq < 0 {
		return s
	}
	key := s[:eq]
	val := s[eq+1:]
	if len(val) >= 2 {
		if (val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
	}
	return key + "=" + val
}

// ---- Process lifecycle -----------------------------------------------------

// shellSplit splits a command string into args using basic shell word splitting
// rules (respects single/double quotes). Used when shell=false.
func shellSplit(s string) []string {
	var args []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == ' ' && !inSingle && !inDouble:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

// start launches a program if it is not currently running.
func (s *kas) start(p *programConfig) {
	s.mu.Lock()
	if rp, ok := s.procs[p.Name]; ok {
		select {
		case <-rp.done:
			// already exited, fall through to start a new one
		default:
			s.mu.Unlock()
			return // already running
		}
	}
	rp := &runningProc{
		done:   make(chan struct{}),
		waitCh: make(chan processStatus, 1),
		status: "starting",
	}
	s.procs[p.Name] = rp
	s.mu.Unlock()

	s.spawn(p, rp, restartInitDelay, 0)
}

// spawn starts the command and runs a watcher goroutine for it.
// backoff is the delay before a failed/crashed restart is attempted.
// retries is the number of restart attempts already made for this program.
func (s *kas) spawn(p *programConfig, rp *runningProc, backoff time.Duration, retries int) {
	var cmd *exec.Cmd
	if p.Shell == "" || p.Shell == "false" || p.Shell == "no" || p.Shell == "none" {
		// Direct exec: split the command string by shell rules.
		args := shellSplit(p.Command)
		if len(args) == 0 {
			s.logger.Printf("program %s: empty command for shell=false", p.Name)
			return
		}
		cmd = exec.Command(args[0], args[1:]...)
	} else {
		// Use the specified shell (e.g. /bin/sh, /bin/bash, /bin/zsh).
		cmd = exec.Command(p.Shell, "-c", p.Command)
	}
	cmd.Env = os.Environ()
	if len(p.Environment) > 0 {
		cmd.Env = append(cmd.Env, p.Environment...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if p.User != "" {
		if u, err := userLookup(p.User); err == nil {
			cmd.SysProcAttr.Credential = &syscall.Credential{Uid: u.Uid, Gid: u.Gid}
		} else {
			s.logger.Printf("program %s: unknown user %q: %v", p.Name, p.User, err)
		}
	}

	if err := wireOutput(cmd, p); err != nil {
		s.logger.Printf("program %s: wire output: %v", p.Name, err)
	}

	if err := cmd.Start(); err != nil {
		s.logger.Printf("program %s: start failed: %v", p.Name, err)
		// No process was created: deliver a synthetic status so supervise can
		// decide whether to retry. Start supervise FIRST so it is guaranteed to
		// be receiving before we send (waitCh has buffer 1 anyway).
		rp.cmd = cmd
		go s.supervise(p, rp, backoff, retries)
		rp.waitCh <- processStatus{pid: 0, exit: 127}
		return
	}
	rp.cmd = cmd

	// Register the pid with the reaper so its exit gets delivered to rp.waitCh.
	s.reaperRegister(cmd.Process.Pid, rp)

	s.mu.Lock()
	rp.startedAt = time.Now()
	rp.status = "running"
	rp.restarts = retries
	s.mu.Unlock()

	s.logger.Printf("program %s: started pid=%d", p.Name, cmd.Process.Pid)
	go s.supervise(p, rp, backoff, retries)
}

// supervise waits for the reaper-delivered exit status, applies autorestart
// with exponential backoff, and signals done.
func (s *kas) supervise(p *programConfig, rp *runningProc, backoff time.Duration, retries int) {
	var st processStatus
	// Guard against the rare race where the child exits between cmd.Start() and
	// reaperRegister, causing the reaper to reap it as an orphan and never
	// deliver a status. Probe the process periodically; if it is gone, synthesize.
	var got bool
	for !got {
		select {
		case st = <-rp.waitCh:
			got = true
		case <-time.After(2 * time.Second):
			// Probe liveness. Signal 0 does not kill the process.
			if rp.cmd == nil || rp.cmd.Process == nil {
				st = processStatus{exit: 127}
				got = true
				continue
			}
			if err := rp.cmd.Process.Signal(syscall.Signal(0)); err != nil {
				// Process no longer exists; treat as exited (status unknown).
				s.logger.Printf("program %s: process vanished before status delivered", p.Name)
				if rp.cmd != nil && rp.cmd.Process != nil {
					s.reaperUnregister(rp.cmd.Process.Pid)
				}
				st = processStatus{exit: -1}
				got = true
			}
		}
	}
	exitCode := st.exit

	rp.stopMu.Lock()
	intentionalStop := rp.stopped
	rp.stopMu.Unlock()

	if intentionalStop {
		s.logger.Printf("program %s: stopped (exit=%d)", p.Name, exitCode)
		s.mu.Lock()
		rp.status = "stopped"
		s.mu.Unlock()
		close(rp.done)
		return
	}
	s.logger.Printf("program %s: exited (exit=%d)", p.Name, exitCode)
	close(rp.done)

	s.mu.Lock()
	rp.status = "stopped"
	s.mu.Unlock()

	// A finished "once" program may unblock dependents; persist its completion
	// so it is not re-run after a kas restart (or container restart, since
	// /var/run is tmpfs and survives restarts but not recreates), then trigger
	// a reconcile. Any exit (success or failure) counts as "ran".
	if p.Type == "once" {
		s.mu.Lock()
		s.ranOnce[p.Name] = true
		s.mu.Unlock()
		markRanOnce(p.Name)
		select {
		case s.depCh <- struct{}{}:
		default:
		}
	}

	// Read the LATEST config (autorestart/action/command may have changed).
	s.mu.Lock()
	cur := s.progs[p.Name]
	// Only this supervise owns rp; if procs[name] is no longer rp, a newer
	// incarnation (from reconcile/start) has already taken over — bail out.
	if cur == nil || s.procs[p.Name] != rp {
		s.mu.Unlock()
		return
	}
	wantRestart := cur.Autorestart && cur.Autostart && cur.Command == p.Command
	// Enforce max_retries: if the cap is reached, stop retrying and leave the
	// program down until a config reload (which resets the counter via a fresh
	// start). max_retries == 0 means retry forever.
	if wantRestart && cur.MaxRetries > 0 && retries >= cur.MaxRetries {
		s.logger.Printf("program %s: giving up after %d retries (max_retries=%d)", p.Name, retries, cur.MaxRetries)
		wantRestart = false
	}
	s.mu.Unlock()
	if !wantRestart {
		return
	}

	// Determine the delay before the next attempt. A configured restart_delay
	// (fixed seconds) takes precedence; otherwise use exponential backoff.
	var delay time.Duration
	if cur.RestartDelay > 0 {
		delay = cur.RestartDelay
	} else {
		delay = backoff
	}
	next := backoff * 2
	if next > restartMaxDelay {
		next = restartMaxDelay
	}
	time.Sleep(delay)

	// Re-check ownership after the sleep: reconcile may have replaced us, or
	// autorestart/action/command/max_retries/restart_delay may have changed.
	s.mu.Lock()
	if s.progs[p.Name] == nil || s.procs[p.Name] != rp {
		s.mu.Unlock()
		return
	}
	cur2 := s.progs[p.Name]
	if !cur2.Autorestart || !cur2.Autostart || cur2.Command != p.Command {
		s.mu.Unlock()
		return
	}
	// Re-evaluate max_retries against the latest config.
	if cur2.MaxRetries > 0 && retries >= cur2.MaxRetries {
		s.mu.Unlock()
		s.logger.Printf("program %s: giving up after %d retries (max_retries=%d)", p.Name, retries, cur2.MaxRetries)
		return
	}
	// Build a fresh runningProc for the new incarnation.
	nrp := &runningProc{
		done:   make(chan struct{}),
		waitCh: make(chan processStatus, 1),
	}
	s.procs[p.Name] = nrp
	s.mu.Unlock()
	s.spawn(p, nrp, next, retries+1)
}

// stop terminates a program and marks it so autorestart is suppressed.
func (s *kas) stop(name string) {
	s.mu.Lock()
	rp, ok := s.procs[name]
	wait := defaultStopWait
	if cfg, cfgOK := s.progs[name]; cfgOK && cfg.StopWaitSecs > 0 {
		wait = cfg.StopWaitSecs
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	rp.stopMu.Lock()
	rp.stopped = true
	rp.stopMu.Unlock()
	s.terminate(rp, wait)
}

// terminate sends SIGTERM to the process group, then SIGKILL after wait.
// If the process never started, it just closes done so callers don't block.
func (s *kas) terminate(rp *runningProc, wait time.Duration) {
	if rp.cmd == nil || rp.cmd.Process == nil {
		// No real process: ensure done is closed so waiters don't hang.
		select {
		case <-rp.done:
		default:
			close(rp.done)
		}
		return
	}
	pgid, err := syscall.Getpgid(rp.cmd.Process.Pid)
	if err != nil {
		pgid = rp.cmd.Process.Pid
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	select {
	case <-rp.done:
	case <-time.After(wait):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-rp.done
	}
}

// ---- Reconciliation --------------------------------------------------------

// reconcile brings the running process set in line with the given config.
//
//   - Programs whose config changed (command/env/...) are restarted.
//   - Programs present in new config with autostart=true are ensured running.
//   - Programs with autostart=false are stopped and left stopped.
//   - Programs no longer in config are stopped and removed.
func (s *kas) reconcile(newProgs map[string]*programConfig) {
	s.mu.Lock()
	oldProgs := s.progs
	s.progs = newProgs
	s.mu.Unlock()

	type entry struct {
		name string
		p    *programConfig
	}
	ordered := make([]entry, 0, len(newProgs))
	for n, p := range newProgs {
		ordered = append(ordered, entry{n, p})
	}
	// Insertion sort by priority then name (small sets expected).
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0; j-- {
			a, b := ordered[j-1], ordered[j]
			if a.p.Priority > b.p.Priority || (a.p.Priority == b.p.Priority && a.name > b.name) {
				ordered[j-1], ordered[j] = b, a
			} else {
				break
			}
		}
	}

	// Stop programs that are gone or whose config changed or that are autostart=false.
	// Done in parallel so a slow stop doesn't delay the whole reload. Use a set
	// to avoid queueing the same program twice (e.g. configChanged + autostart=false).
	type stopJob struct{ name string }
	stopSet := make(map[string]struct{})
	var jobs []stopJob
	addJob := func(name, reason string) {
		if _, dup := stopSet[name]; dup {
			return
		}
		stopSet[name] = struct{}{}
		s.logger.Printf("program %s: %s", name, reason)
		jobs = append(jobs, stopJob{name})
	}
	for name, old := range oldProgs {
		np, present := newProgs[name]
		if !present {
			addJob(name, "removed by config, stopping")
			continue
		}
		if configChanged(old, np) {
			addJob(name, "config changed, restarting")
		}
	}
	// Also stop programs whose autostart flipped to false.
	for _, e := range ordered {
		name, p := e.name, e.p
		if !p.Autostart {
			s.mu.Lock()
			rp, running := s.procs[name]
			s.mu.Unlock()
			if running {
				select {
				case <-rp.done:
				default:
					addJob(name, "autostart=false, stopping")
				}
			}
		}
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			s.stop(name)
		}(j.name)
	}
	wg.Wait()
	// Clean up procs entries for removed programs.
	for _, j := range jobs {
		if _, present := newProgs[j.name]; !present {
			s.mu.Lock()
			delete(s.procs, j.name)
			s.mu.Unlock()
		}
	}

	// Start/restart programs in priority order. Programs with unsatisfied
	// dependencies are skipped this round; they will be retried on the next
	// reload, or when a "once" dependency finishes (supervise triggers a
	// reconcile via depCh).
	for _, e := range ordered {
		name, p := e.name, e.p
		// autostart=false: skip starting (and stopped above).
		if !p.Autostart {
			continue
		}
		s.mu.Lock()
		rp, running := s.procs[name]
		ran := s.ranOnce[name]
		s.mu.Unlock()
		shouldStart := true
		if running {
			select {
			case <-rp.done:
			default:
				shouldStart = false
			}
		}
		if !shouldStart {
			continue
		}
		// "once" programs: skip if already completed (ranOnce), or if a prior
		// incarnation is still tracked in procs (running or just-exited but not
		// yet marked done by supervise). This prevents double-start across
		// concurrent reconciles. Any exit counts as "ran" (success or failure).
		if p.Type == "once" {
			if ran {
				continue
			}
			if running {
				// An entry exists; if it is done but ranOnce is still false the
				// supervise goroutine hasn't finished marking yet — skip and let
				// the next reconcile pick it up.
				continue
			}
		}
		// Check dependencies are satisfied.
		if !s.depsReady(p, newProgs) {
			s.logger.Printf("program %s: waiting for deps %v", name, p.Deps)
			continue
		}
		s.start(p)
	}
}

// depsReady reports whether all dependencies of p are satisfied:
//   - a "long" dep is satisfied once it is running (in procs and not done);
//   - a "once" dep is satisfied once it has completed (ranOnce and done).
//
// An unknown/missing dep is treated as not ready.
func (s *kas) depsReady(p *programConfig, progs map[string]*programConfig) bool {
	for _, dep := range p.Deps {
		dp, ok := progs[dep]
		if !ok {
			return false
		}
		s.mu.Lock()
		rp, running := s.procs[dep]
		ran := s.ranOnce[dep]
		s.mu.Unlock()
		if dp.Type == "once" {
			// once dep is ready only after it has finished.
			if !ran {
				return false
			}
			if running {
				select {
				case <-rp.done:
				default:
					return false
				}
			}
		} else {
			// long dep is ready once it is running.
			if !running {
				return false
			}
			select {
			case <-rp.done:
				return false
			default:
			}
		}
	}
	return true
}

func configChanged(a, b *programConfig) bool {
	if a.Command != b.Command || a.User != b.User {
		return true
	}
	if a.Type != b.Type {
		return true
	}
	if a.MaxRetries != b.MaxRetries || a.RestartDelay != b.RestartDelay {
		return true
	}
	if len(a.Deps) != len(b.Deps) {
		return true
	}
	for i := range a.Deps {
		if a.Deps[i] != b.Deps[i] {
			return true
		}
	}
	if len(a.Environment) != len(b.Environment) {
		return true
	}
	for i := range a.Environment {
		if a.Environment[i] != b.Environment[i] {
			return true
		}
	}
	if a.StdoutLog != b.StdoutLog || a.StderrLog != b.StderrLog {
		return true
	}
	if a.Shell != b.Shell {
		return true
	}
	return false
}

// ---- I/O helpers -----------------------------------------------------------

func wireOutput(cmd *exec.Cmd, p *programConfig) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	go pump(stdout, p.StdoutLog, p.Name, "stdout")
	go pump(stderr, p.StderrLog, p.Name, "stderr")
	return nil
}

// pump copies a child stream. When a logfile is configured it appends there;
// otherwise the line goes straight to the kas console (stdout for the program's
// stdout stream, stderr for the program's stderr stream) so operators see child
// output inline with kas logs. logfile values "AUTO"/"NONE" mean "console".
func pump(r io.ReadCloser, logfile, name, stream string) {
	defer r.Close()

	var f *os.File
	if logfile != "" && logfile != "AUTO" && logfile != "NONE" {
		if err := os.MkdirAll(filepath.Dir(logfile), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "[kas] program %s: cannot create log dir for %s: %v; falling back to console\n", name, logfile, err)
		} else if ff, err := os.OpenFile(logfile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "[kas] program %s: cannot open log file %s: %v; falling back to console\n", name, logfile, err)
		} else {
			f = ff
			defer f.Close()
		}
	}

	// Console target: program stdout -> kas stdout, program stderr -> kas stderr.
	var console *os.File
	if stream == "stderr" {
		console = os.Stderr
	} else {
		console = os.Stdout
	}

	prefix := fmt.Sprintf("[%s:%s] ", name, stream)
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			msg := strings.TrimRight(line, "\n")
			if f != nil {
				fmt.Fprintln(f, prefix+msg)
			} else {
				// No logfile: forward to the kas console verbatim (no [kas] prefix).
				fmt.Fprintln(console, prefix+msg)
			}
		}
		if err != nil {
			return
		}
	}
}

// ---- User lookup -----------------------------------------------------------

type userInfo struct{ Uid, Gid uint32 }

func userLookup(name string) (*userInfo, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("user lookup only on linux")
	}
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 4 || fields[0] != name {
			continue
		}
		uid, err1 := strconv.ParseUint(fields[2], 10, 32)
		gid, err2 := strconv.ParseUint(fields[3], 10, 32)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("bad uid/gid for %s", name)
		}
		return &userInfo{Uid: uint32(uid), Gid: uint32(gid)}, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read /etc/passwd: %w", err)
	}
	return nil, fmt.Errorf("user %q not found", name)
}

// ---- File watching ---------------------------------------------------------

func fileMtime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// ---- PID 1 responsibilities ------------------------------------------------

// shutdown stops all managed programs gracefully (SIGTERM then SIGKILL), in
// parallel. It is the only signal-driven exit path: SIGTERM/SIGINT trigger it.
func (s *kas) shutdown() {
	s.mu.Lock()
	names := make([]string, 0, len(s.procs))
	for n := range s.procs {
		names = append(names, n)
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			s.mu.Lock()
			rp, ok := s.procs[name]
			wait := defaultStopWait
			if cfg, ok := s.progs[name]; ok && cfg.StopWaitSecs > 0 {
				wait = cfg.StopWaitSecs
			}
			s.mu.Unlock()
			if !ok {
				return
			}
			rp.stopMu.Lock()
			rp.stopped = true
			rp.stopMu.Unlock()
			s.terminate(rp, wait)
		}(n)
	}
	wg.Wait()
}

// ---- IPC: unix socket for `kas ps` ----------------------------------------

// procInfo is the status snapshot sent to `kas ps`.
type procInfo struct {
	Name     string
	PID      int
	Type     string
	Status   string
	Restarts int
	Uptime   string
	Priority int
}

// serveSock listens on sockPath and answers queries from `kas ps` and
// `kas reload`. The client sends a single line: "ps" or "reload".
func (s *kas) serveSock(sockPath string, hupCh chan<- struct{}) {
	_ = os.MkdirAll(filepath.Dir(sockPath), 0o755)
	_ = os.Remove(sockPath) // stale socket from a previous run
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		s.logger.Printf("warn: cannot listen on %s: %v (kas ps/reload will be unavailable)", sockPath, err)
		return
	}
	defer l.Close()
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		s.handleConn(conn, hupCh)
	}
}

// handleConn reads a one-line command and dispatches to ps, reload, restart, start, or stop.
func (s *kas) handleConn(conn net.Conn, hupCh chan<- struct{}) {
	defer conn.Close()
	cmd, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && err != io.EOF {
		return
	}
	cmd = strings.TrimSpace(cmd)
	parts := strings.SplitN(cmd, " ", 2)
	switch parts[0] {
	case "ps":
		s.handlePS(conn)
	case "reload":
		select {
		case hupCh <- struct{}{}:
			fmt.Fprintln(conn, "reload triggered")
		default:
			fmt.Fprintln(conn, "reload already pending")
		}
	case "restart", "start", "stop":
		if len(parts) < 2 || parts[1] == "" {
			fmt.Fprintf(conn, "error: %s requires a service name\n", parts[0])
			return
		}
		name := parts[1]
		// Reject names containing whitespace/newlines: they can never match a
		// configured [program:NAME] section (which is a single line) and a
		// newline would let a crafted client inject a second command line.
		if strings.ContainsAny(name, " \t\n\r") {
			fmt.Fprintf(conn, "error: invalid service name %q\n", name)
			return
		}
		switch parts[0] {
		case "restart":
			s.handleRestart(conn, name)
		case "start":
			s.handleStart(conn, name)
		case "stop":
			s.handleStop(conn, name)
		}
	default:
		fmt.Fprintln(conn, "unknown command (use: ps | reload | restart <name> | start <name> | stop <name>)")
	}
}

// handleStart starts the named service if it is not already running.
func (s *kas) handleStart(conn net.Conn, name string) {
	s.mu.Lock()
	cfg, ok := s.progs[name]
	ran := s.ranOnce[name]
	s.mu.Unlock()
	if !ok {
		fmt.Fprintf(conn, "error: unknown service %q\n", name)
		return
	}
	if cfg.Type == "once" && ran {
		fmt.Fprintf(conn, "error: %q is a once task already initialized\n", name)
		return
	}
	s.logger.Printf("program %s: start requested", name)
	s.start(cfg)
	fmt.Fprintf(conn, "started %s\n", name)
}

// handleStop stops the named service.
func (s *kas) handleStop(conn net.Conn, name string) {
	s.mu.Lock()
	_, ok := s.progs[name]
	s.mu.Unlock()
	if !ok {
		fmt.Fprintf(conn, "error: unknown service %q\n", name)
		return
	}
	s.logger.Printf("program %s: stop requested", name)
	s.stop(name)
	fmt.Fprintf(conn, "stopped %s\n", name)
}

// handleRestart stops and then starts the named service.
func (s *kas) handleRestart(conn net.Conn, name string) {
	s.mu.Lock()
	cfg, ok := s.progs[name]
	s.mu.Unlock()
	if !ok {
		fmt.Fprintf(conn, "error: unknown service %q\n", name)
		return
	}
	if cfg.Type == "once" && s.ranOnce[name] {
		fmt.Fprintf(conn, "error: %q is a once task already initialized (not restartable)\n", name)
		return
	}
	s.logger.Printf("program %s: restart requested", name)
	s.stop(name)
	s.start(cfg)
	fmt.Fprintf(conn, "restarted %s\n", name)
}

// handlePS builds a status snapshot and writes it as a tab-separated table.
func (s *kas) handlePS(conn net.Conn) {
	s.mu.Lock()
	infos := make([]procInfo, 0, len(s.progs))
	for name, cfg := range s.progs {
		pi := procInfo{Name: name, Type: cfg.Type, Status: "stopped", Priority: cfg.Priority}
		if rp, ok := s.procs[name]; ok {
			pi.Restarts = rp.restarts
			pi.Status = rp.status
			if rp.cmd != nil && rp.cmd.Process != nil {
				pi.PID = rp.cmd.Process.Pid
			}
			if !rp.startedAt.IsZero() && pi.Status == "running" {
				pi.Uptime = time.Since(rp.startedAt).Round(time.Second).String()
			}
		}
		// once programs that already ran show as "initialized" (completed once-init).
		if cfg.Type == "once" && s.ranOnce[name] {
			pi.Status = "initialized"
		}
		infos = append(infos, pi)
	}
	s.mu.Unlock()

	// Sort by priority then name for stable output.
	for i := 1; i < len(infos); i++ {
		for j := i; j > 0; j-- {
			a, b := infos[j-1], infos[j]
			if a.Priority > b.Priority || (a.Priority == b.Priority && a.Name > b.Name) {
				infos[j-1], infos[j] = infos[j], infos[j-1]
			} else {
				break
			}
		}
	}

	w := tabwriter.NewWriter(conn, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPID\tTYPE\tSTATUS\tRESTARTS\tUPTIME")
	for _, pi := range infos {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%d\t%s\n", pi.Name, pi.PID, pi.Type, pi.Status, pi.Restarts, pi.Uptime)
	}
	w.Flush()
}

// psClient connects to the kas unix socket and prints the process table.
func psClient(sockPath string) int {
	return runClient(sockPath, "ps")
}

// reloadClient connects to the kas unix socket and triggers a config reload.
func reloadClient(sockPath string) int {
	return runClient(sockPath, "reload")
}

// namedCmdClient sends a "<cmd> <name>" request. It rejects names containing
// whitespace/newlines so a crafted name cannot inject a second command line
// over the socket protocol (which is line-oriented).
func namedCmdClient(sockPath, cmd, name string) int {
	if strings.ContainsAny(name, " \t\n\r") {
		fmt.Fprintf(os.Stderr, "error: invalid service name %q (no whitespace allowed)\n", name)
		return 2
	}
	return runClient(sockPath, cmd+" "+name)
}

// restartClient connects to the kas unix socket and restarts a service.
func restartClient(sockPath, name string) int {
	return namedCmdClient(sockPath, "restart", name)
}

// startClient connects to the kas unix socket and starts a service.
func startClient(sockPath, name string) int {
	return namedCmdClient(sockPath, "start", name)
}

// stopClient connects to the kas unix socket and stops a service.
func stopClient(sockPath, name string) int {
	return namedCmdClient(sockPath, "stop", name)
}

// runClient sends a command to the kas socket and copies the response to stdout.
func runClient(sockPath, cmd string) int {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot connect to kas at %s: %v\n", sockPath, err)
		fmt.Fprintf(os.Stderr, "is kas running? (socket: %s)\n", sockPath)
		return 1
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		fmt.Fprintf(os.Stderr, "send failed: %v\n", err)
		return 1
	}
	io.Copy(os.Stdout, conn)
	return 0
}

// ---- argument parsing for subcommands --------------------------------------

// defaultSock resolves the IPC socket path from $SOCK_PATH, falling back to
// defaultSockPath when unset. Shared by the client subcommands and the daemon
// so they always agree on the default.
func defaultSock() string {
	if p := os.Getenv("SOCK_PATH"); p != "" {
		return p
	}
	return defaultSockPath
}

// parseSubArgs scans args and extracts the -s socket flag plus the remaining
// positional tokens (the service NAME for start/stop/restart). Unlike the
// standard flag package, -s may appear anywhere: before, after, or between
// positional args, so all of these work:
//
//	kas ps -s /p            kas -s /p ps
//	kas start web -s /p     kas -s /p start web
//	kas start -s /p web     kas start web -s=/p
//
// -s value precedence: command-line -s > $SOCK_PATH > defaultSockPath.
// It returns the resolved socket path and the collected positional args. It
// calls os.Exit on a malformed -s (missing or empty value).
func parseSubArgs(args []string) (sockPath string, positional []string) {
	sockPath = defaultSock()
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-s":
			// Reject a missing value, or a value that itself looks like a flag
			// (e.g. `kas -s -c cfg` would otherwise eat "-c" as the socket path).
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintln(os.Stderr, "error: -s requires a value")
				os.Exit(2)
			}
			sockPath = args[i+1]
			i++ // consume the value
		case strings.HasPrefix(a, "-s="):
			sockPath = strings.TrimPrefix(a, "-s=")
		default:
			positional = append(positional, a)
		}
	}
	if sockPath == "" {
		fmt.Fprintln(os.Stderr, "error: -s value must not be empty")
		os.Exit(2)
	}
	return sockPath, positional
}

// runSubcommand dispatches the named client subcommand using the already-parsed
// socket path and positional args. It returns true when name is a recognized
// subcommand (and has been executed via os.Exit); false otherwise so the caller
// can decide what to do with an unrecognized token (flag -> daemon; otherwise
// an unknown-subcommand error). Keeping the recognition and dispatch in a
// single switch avoids a separate subcommand set that could drift out of sync.
func runSubcommand(name, sockPath string, args []string) bool {
	requireName := func(usage string) string {
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(1)
		}
		return args[0]
	}
	switch name {
	case "ps":
		os.Exit(psClient(sockPath))
	case "reload":
		os.Exit(reloadClient(sockPath))
	case "restart":
		os.Exit(restartClient(sockPath, requireName("usage: kas restart NAME [-s sock]")))
	case "start":
		os.Exit(startClient(sockPath, requireName("usage: kas start NAME [-s sock]")))
	case "stop":
		os.Exit(stopClient(sockPath, requireName("usage: kas stop NAME [-s sock]")))
	}
	return false // not a subcommand
}

// ---- main ------------------------------------------------------------------

func main() {
	args := os.Args[1:]

	// No arguments at all: print help and exit. The daemon must be started
	// explicitly with -c so an accidental bare `kas` does not silently take
	// over PID 1 duties.
	if len(args) == 0 {
		printUsage(os.Stdout)
		os.Exit(0)
	}

	// --- Client subcommands -------------------------------------------------
	// The subcommand and -s may appear in either order. parseSubArgs strips -s
	// (wherever it is) and collects the remaining positional tokens; the first
	// of those is the subcommand. Examples that all resolve identically:
	//
	//	kas ps -s /p           -> sub="ps",  args=[]
	//	kas -s /p ps           -> sub="ps",  args=[]
	//	kas start web -s /p    -> sub="start", args=["web"]
	//	kas -s /p start web    -> sub="start", args=["web"]
	sock, rest := parseSubArgs(args)
	if len(rest) > 0 {
		if runSubcommand(rest[0], sock, rest[1:]) {
			return
		}
		// rest[0] is not a recognized subcommand. If it looks like a flag
		// (e.g. "-c"), fall through to daemon mode so `kas -c cfg` and
		// `kas -s /p -c cfg` still work. Otherwise it is an unknown
		// subcommand (e.g. `kas foo`) and we must NOT silently start the
		// daemon: report it and exit.
		if !strings.HasPrefix(rest[0], "-") {
			fmt.Fprintf(os.Stderr, "error: unknown command %q (use: ps | reload | restart <name> | start <name> | stop <name>)\n", rest[0])
			os.Exit(2)
		}
	}

	// --- Daemon (PID 1) mode ------------------------------------------------
	// Not a subcommand invocation: run as PID 1. The daemon requires an explicit
	// -c flag (which may carry no value, in which case the default config path
	// /etc/kas.ini is used) so that bare `kas` never silently starts the manager.
	// parseSubArgs has already resolved -s into sock; pass it along with the raw
	// args so runDaemon can locate -c.
	runDaemon(args, sock)
}

// printUsage writes a detailed help message to w. It documents both the
// daemon mode (entered via -c) and the client subcommands, so a single `kas -h`
// or bare `kas` gives the operator everything they need.
func printUsage(w io.Writer) {
	fmt.Fprintf(w, `kas - a tiny PID-1 process manager for containers

Usage:
  kas -c [PATH] [-s SOCK]     Run as the PID-1 daemon (manages programs)
  kas -h | --help              Show this help and exit
  kas <subcommand> [NAME] [-s SOCK]
                               Send a control command to a running daemon

Daemon mode (PID 1):
  -c [PATH]   REQUIRED. Path to the kas ini config file. When PATH is omitted
              the default %q is used. This flag must be present to start the
              daemon, so a bare 'kas' never silently takes over PID 1.
  -s SOCK     Path to the unix socket used for IPC (default %q or $SOCK_PATH).

  The config uses [program:NAME] sections (supervisor-style) with directives:
    command, autostart, autorestart, stopwaitsecs, priority, user, environment,
    stdout_logfile, stderr_logfile, type (once|long), depends, max_retries,
    restart_delay, shell.

Client subcommands (talk to a running daemon via the socket):
  ps                          Print the managed process table
  reload                      Re-read the config and reconcile
  start NAME                  Start a service by name
  stop NAME                   Stop a service by name
  restart NAME                Stop then start a service by name

  -s SOCK     Override the IPC socket path for a single command.

Examples:
  kas -c                       Start daemon with /etc/kas.ini
  kas -c /path/to/kas.ini      Start daemon with a custom config
  kas -c -s /tmp/kas.sock      Start daemon with a custom socket
  kas ps                       Show the process table
  kas -s /tmp/kas.sock ps      Query a daemon on a non-default socket
  kas reload                   Reload config without restarting kas

Signals:
  SIGHUP                      Reload the config (same as 'kas reload')
  SIGTERM / SIGINT            Gracefully stop all programs and exit
`, defaultConfigPath, defaultSockPath)
}

// runDaemon starts kas as the PID-1 process manager. The socket path is taken
// from sockPath (already resolved by parseSubArgs, which honors -s and
// $SOCK_PATH). The -c flag selects the config file and is REQUIRED to start
// the daemon, but it may be given without a value (`kas -c`), in which case
// the default config path /etc/kas.ini is used. This prevents an accidental
// bare `kas` from silently taking over PID 1 duties.
func runDaemon(args []string, sockPath string) {
	// Locate -c (and -h) among the raw args. parseSubArgs has already stripped
	// -s, but we scan the original args here so -c may appear in any position.
	cfgPath := defaultConfigPath
	var hasC bool
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c":
			hasC = true
			// -c may carry no value: only consume the next token as the value if
			// it exists and does not look like another flag.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				cfgPath = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-c="):
			hasC = true
			if v := strings.TrimPrefix(a, "-c="); v != "" {
				cfgPath = v
			} else {
				cfgPath = defaultConfigPath
			}
		case a == "-h" || a == "--help" || a == "-help":
			printUsage(os.Stdout)
			os.Exit(0)
		}
	}

	if !hasC {
		// The daemon must be started explicitly with -c. Without it, print help
		// (mirroring `kas` with no args) instead of silently starting.
		fmt.Fprintln(os.Stderr, "error: -c is required to start the daemon (use `kas -c` for the default config, or `kas -h` for help)")
		printUsage(os.Stderr)
		os.Exit(2)
	}

	s := newKas() // also starts the single child-reaping goroutine (tini role)

	// hupCh carries reload triggers from `kas reload` and SIGHUP to the main loop.
	hupCh := make(chan struct{}, 1)

	// Start the IPC socket server for `kas ps` and `kas reload`.
	go s.serveSock(sockPath, hupCh)

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGHUP:
				// Immediate reload via the dedicated channel.
				s.logger.Printf("received SIGHUP: reload trigger")
				select {
				case hupCh <- struct{}{}:
				default:
				}
			default:
				s.logger.Printf("received %v: shutting down", sig)
				s.shutdown()
				_ = os.Remove(sockPath)
				os.Exit(0)
			}
		}
	}()

	// Initial config load.
	s.logger.Printf("kas started, config=%s", cfgPath)
	loadConfig := func(reason string) map[string]*programConfig {
		progs, err := parseConfig(cfgPath)
		if err != nil {
			s.logger.Printf("config %s error: %v", reason, err)
			return nil
		}
		s.logger.Printf("config %s (mtime=%s)", reason, fileMtime(cfgPath).Format(time.RFC3339))
		return progs
	}
	var lastProgs map[string]*programConfig
	if lastProgs = loadConfig("loaded"); lastProgs != nil {
		s.reconcile(lastProgs)
	}

	for {
		select {
		case <-hupCh:
			if progs := loadConfig("reloaded"); progs != nil {
				lastProgs = progs
				s.reconcile(progs)
			}
		case <-s.depCh:
			// A "once" program finished; re-reconcile with the latest config so
			// programs waiting on it as a dependency can start.
			if lastProgs != nil {
				s.reconcile(lastProgs)
			}
		}
	}
}
