package pkg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestProcessStartDir Start 携带 dir 时，子进程的工作目录落在该目录
// （svc 启动的 command 必须在服务 home 里解析相对路径，而不是 kvs 的 cwd）。
func TestProcessStartDir(t *testing.T) {
	home := t.TempDir()
	out := filepath.Join(t.TempDir(), "pwd.txt")
	// Process 按 strings.Fields 切分命令（不支持引号），用脚本文件写出 pwd。
	script := filepath.Join(home, "pwd.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd > "+out+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Process{Name: "t"}
	if err := p.Start("t", home, script); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil {
			if got := strings.TrimSpace(string(b)); got != home {
				t.Fatalf("child cwd = %q, want %q", got, home)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("child never wrote pwd output")
}

// TestRunServiceStartupDir 生命周期脚本（once/init/stop shell、actions）同样
// 落在传入的目录里执行。
func TestRunServiceStartupDir(t *testing.T) {
	home := t.TempDir()
	out := filepath.Join(t.TempDir(), "pwd.txt")
	if err := RunServiceStartup(home, "pwd > "+out); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != home {
		t.Fatalf("script cwd = %q, want %q", got, home)
	}
}

// TestProcessStdoutCapture 配置 upstream_token 后，子进程 stdout 里的
// token 行按 pattern 前缀截取并存入 sc + env。
func TestProcessStdoutCapture(t *testing.T) {
	home := t.TempDir()
	sc := &ServiceConfig{Name: "cap1"}
	sc.UpstreamTokenPattern, sc.UpstreamTokenKey = "dsh web: http://127.0.0.1:7531/?token=", "token"
	script := filepath.Join(home, "run.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\necho starting\necho \"dsh web: http://127.0.0.1:7531/?token=SECRET123\"\necho done\n",
	), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Process{Name: "cap1"}
	p.SetStdoutLine(func(line string) bool {
		if !strings.HasPrefix(line, sc.UpstreamTokenPattern) {
			return false
		}
		tok := strings.TrimSpace(line[len(sc.UpstreamTokenPattern):])
		if tok == "" {
			return false
		}
		sc.SetUpstreamToken(tok)
		return true
	})
	if err := p.Start("cap1", home, script); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sc.UpstreamToken() == "SECRET123" {
			if got := os.Getenv(svcEnvKey("cap1", "UPSTREAM_TOKEN")); got != "SECRET123" {
				t.Fatalf("env KVS_SVC_CAP1_UPSTREAM_TOKEN = %q, want SECRET123", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("token not captured, got %q", sc.UpstreamToken())
}

// TestProcessStdoutCaptureTimeout 截取窗口关闭后，即使出现匹配行也不再捕获
// （防止后端日志晚到的同形行污染 token）。
func TestProcessStdoutCaptureTimeout(t *testing.T) {
	old := stdoutCaptureTimeout
	stdoutCaptureTimeout = 200 * time.Millisecond
	defer func() { stdoutCaptureTimeout = old }()

	home := t.TempDir()
	sc := &ServiceConfig{Name: "cap2"}
	sc.UpstreamTokenPattern, sc.UpstreamTokenKey = "token line: ", "tk"
	script := filepath.Join(home, "run.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\nsleep 0.6\necho \"token line: LATE\"\n",
	), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Process{Name: "cap2"}
	p.SetStdoutLine(func(line string) bool {
		if !strings.HasPrefix(line, sc.UpstreamTokenPattern) {
			return false
		}
		sc.SetUpstreamToken(strings.TrimSpace(line[len(sc.UpstreamTokenPattern):]))
		return true
	})
	if err := p.Start("cap2", home, script); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(900 * time.Millisecond)
	if got := sc.UpstreamToken(); got != "" {
		t.Fatalf("token captured after window closed: %q", got)
	}
}

// TestProcEnvAndPPID /proc 核验工具：env 标记识别（防 pid 回收误伤）与
// PPID 解析。
func TestProcEnvAndPPID(t *testing.T) {
	marker := "KVS_SVC_PT_MANAGED=1"
	marked := exec.Command("sleep", "5")
	marked.Env = append(os.Environ(), marker)
	if err := marked.Start(); err != nil {
		t.Fatal(err)
	}
	defer marked.Process.Signal(syscall.SIGKILL)
	// /proc/<pid>/environ 在 exec 后存在短暂可见性窗口，轮询等待。
	ok := false
	for i := 0; i < 20 && !ok; i++ {
		ok = procEnvHas(marked.Process.Pid, marker)
		if !ok {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !ok {
		t.Error("带标记进程应命中 procEnvHas")
	}
	plain := exec.Command("sleep", "5")
	plain.Env = os.Environ()
	if err := plain.Start(); err != nil {
		t.Fatal(err)
	}
	defer plain.Process.Signal(syscall.SIGKILL)
	if procEnvHas(plain.Process.Pid, marker) {
		t.Error("无标记进程不应命中 procEnvHas（pid 回收防护）")
	}
	if ppid := procPPID(plain.Process.Pid); ppid != os.Getpid() {
		t.Errorf("procPPID = %d, want %d", ppid, os.Getpid())
	}
}

// TestSweepOrphansKillsRealOrphan 构造真实孤儿（中间 sh 退出，sleep 被 init
// 收养、PPID=1、带 MANAGED 标记）：SweepOrphans 必须击杀并销账。
func TestSweepOrphansKillsRealOrphan(t *testing.T) {
	home := t.TempDir()
	pidFile := filepath.Join(home, "kvs-svc.json")
	orphanOut := filepath.Join(t.TempDir(), "pid")
	mk := exec.Command("sh", "-c", "sleep 30 & echo $! > "+orphanOut)
	mk.Env = append(os.Environ(), "KVS_SVC_SORPH_MANAGED=1")
	if err := mk.Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(orphanOut)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))

	m := &ServiceManager{svcs: map[string]*managedService{}, pids: map[string]int{}}
	m.SetHomeDir(home)
	m.registerPid("sorph", pid)

	deadline := time.Now().Add(3 * time.Second)
	for procPPID(pid) != 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	m.SweepOrphans()
	deadline = time.Now().Add(5 * time.Second)
	for procAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if procAlive(pid) {
		t.Fatalf("orphan %d still alive after sweep", pid)
	}
	if _, ok := func() (int, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		v, ok := m.pids["sorph"]
		return v, ok
	}(); ok {
		t.Error("registry entry should be forgotten after sweep")
	}
	_ = pidFile
}

// TestStopOneEnsureDead StopOne 正常终止后对托管 pid 补刀并销账。
func TestStopOneEnsureDead(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &ServiceManager{svcs: map[string]*managedService{}, pids: map[string]int{}}
	m.SetHomeDir(home)
	sc := &ServiceConfig{Name: "st1", Home: home, Command: script}
	m.Register(map[string]*ServiceConfig{"st1": sc})
	if err := m.StartProc(sc); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	pid := m.pids["st1"]
	m.mu.Unlock()
	if pid <= 0 || !procAlive(pid) {
		t.Fatalf("child pid %d not alive", pid)
	}
	if !procEnvHas(pid, "KVS_SVC_ST1_MANAGED=1") {
		t.Error("子进程应继承 MANAGED 标记")
	}
	m.StopOne("st1", false)
	if procAlive(pid) {
		t.Errorf("child %d still alive after StopOne", pid)
	}
	m.mu.Lock()
	_, ok := m.pids["st1"]
	m.mu.Unlock()
	if ok {
		t.Error("registry entry should be forgotten after StopOne")
	}
}
