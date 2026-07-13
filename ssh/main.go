// sshp：基于登录名进行 SSH 分流、中继与代理。
//
// 登录格式：server-name[/user-name][:password]@proxy-host
//   - server-name 既可以是单纯主机名，也可以是 host-port-ssvc{snum} 复合形式
//   - user-name 默认值为 user
//   - password 可选：内嵌于用户名（冒号分隔）或由客户端在认证阶段输入
//
// 密码两种指定方式（二选一）：
//   - 内嵌：server-name/user:password  — 无需后输入，认证直接通过
//   - 后输入：server-name/user         — 认证阶段要求客户端输入密码
//
// 代理通过用户名解析出目标地址后，建立到目标的 SSH 连接，
// 并在客户端与目标之间双向转发 channel 与 global request。
//
// 测试：
//
//	服务: TARGET_ADDR='{host}:22' go run main.go
//	内嵌密码: ssh -o StrictHostKeyChecking=no x.x.x.x/root:pass@127.0.0.1
//	后输入密码: ssh -o StrictHostKeyChecking=no x.x.x.x/root@127.0.0.1
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// =============================================================================
// Proxy 代理服务核心结构
// =============================================================================

// dialTimeout 拨号超时时间。
const dialTimeout = 30 * time.Second

// tcpKeepAlive 是 TCP 层面的 KeepAlive 周期。
// 内网防火墙/NAT 可能在 5-15 分钟无数据后断开空闲连接，
// 设置为 30 秒可确保在绝大多数中间设备超时前发送探测包。
const tcpKeepAlive = 30 * time.Second

// passKey 是 Permissions.Extensions 中存放客户端输入密码的 key。
const passKey = "pass"

// Proxy 表示一个 SSH 代理服务实例。
type Proxy struct {
	KeysFolder string        // 主机密钥目录，默认 /etc/ssh
	ListenAddr string        // 监听地址，默认 :22
	TargetAddr string        // 目标地址模板，支持 {host}/{port}/{ssvc}/{snum} 占位符
	Listener   net.Listener  // TCP 监听器
	wchan      chan struct{} // 服务停止通知
}

// Wait 阻塞等待服务停止。
func (p *Proxy) Wait() {
	if p.wchan != nil {
		<-p.wchan
	}
}

// Stop 关闭代理服务。
func (p *Proxy) Stop() error {
	if p.Listener != nil {
		return p.Listener.Close()
	}
	return nil
}

// Start 启动 SSH 代理服务。
func (p *Proxy) Start() error {
	if p.Listener != nil {
		return errors.New("sshp is already running")
	}
	conf, err := p.InitConf()
	if err != nil {
		return err
	}
	// 启动 TCP 监听
	p.Listener, err = net.Listen("tcp", p.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", p.ListenAddr, err)
	}
	// 重置停止通知
	if p.wchan != nil {
		close(p.wchan)
	}
	p.wchan = make(chan struct{})
	// 监听连接
	go p.serve(conf)
	return nil
}

// serve 接受客户端连接并分发给 HandleSshConn 处理。
func (p *Proxy) serve(conf *ssh.ServerConfig) {
	defer p.Listener.Close()
	slog.Info("sshp is listening", "addr", p.ListenAddr)
	for {
		conn, err := p.Listener.Accept()
		if err != nil {
			// 监听器已关闭，服务停止
			if errors.Is(err, net.ErrClosed) {
				slog.Info("sshp is closed")
				if p.wchan != nil {
					close(p.wchan)
					p.wchan = nil
				}
				return
			}
			slog.Error("failed to accept incoming connection", "err", err)
			continue
		}
		go p.HandleSshConn(conn, conf)
	}
}

// InitConf 初始化 SSH 服务端配置与默认参数。
func (p *Proxy) InitConf() (*ssh.ServerConfig, error) {
	// 服务配置：双认证模式
	//   - 用户名内嵌密码（含 ':'）：NoClientAuthCallback 直接放行
	//   - 用户名无密码（不含 ':'）：NoClientAuthCallback 返回 PartialSuccessError，
	//     要求客户端继续密码认证，由 PasswordCallback 收集密码
	// passwordCb 是密码认证回调，对无内嵌密码的用户名收集客户端输入的密码。
	// 被 NoClientAuthCallback 的 PartialSuccessError.Next 复用。
	passwordCb := func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if strings.Contains(meta.User(), ":") {
			// 内嵌密码用户不应走到这里，拒绝以防混淆
			return nil, errors.New("password already embedded in username")
		}
		// 通过 Permissions.Extensions 将密码传递到 HandleSshConn，
		// 避免使用全局 map（无垃圾数据残留风险）。
		return &ssh.Permissions{Extensions: map[string]string{passKey: string(password)}}, nil
	}
	conf := &ssh.ServerConfig{
		NoClientAuth: true,
		NoClientAuthCallback: func(meta ssh.ConnMetadata) (*ssh.Permissions, error) {
			// 用户名含 ':' 表示密码已内嵌，直接通过
			if strings.Contains(meta.User(), ":") {
				return nil, nil
			}
			// 无内嵌密码，返回 PartialSuccessError 要求客户端继续密码认证。
			// 必须在 Next 中显式传入 PasswordCallback，否则后续密码认证不可用。
			return nil, &ssh.PartialSuccessError{
				Next: ssh.ServerAuthCallbacks{
					PasswordCallback: passwordCb,
				},
			}
		},
		PasswordCallback: passwordCb,
	}
	// 加载主机密钥, DSA 密钥已被弃用, ssh-keygen -A -f "__keys/"
	if p.KeysFolder == "" {
		p.KeysFolder = "/etc/ssh"
	}
	for _, name := range []string{"ssh_host_rsa_key", "ssh_host_ecdsa_key", "ssh_host_ed25519_key"} {
		if err := p.AddHostKey(conf, p.KeysFolder+"/"+name); err != nil {
			return nil, err
		}
	}
	// 服务监听地址
	if p.ListenAddr == "" {
		p.ListenAddr = ":22"
	}
	// 目标地址模板
	if p.TargetAddr == "" {
		p.TargetAddr = "{host}:22"
	}
	return conf, nil
}

// AddHostKey 从文件加载私钥并添加到服务端配置。
func (p *Proxy) AddHostKey(conf *ssh.ServerConfig, file string) error {
	bts, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("failed to read private key file %s: %w", file, err)
	}
	key, err := ssh.ParsePrivateKey(bts)
	if err != nil {
		return fmt.Errorf("failed to parse private key %s: %w", file, err)
	}
	conf.AddHostKey(key)
	return nil
}

// setKeepAlive 在 net.Conn 上启用 TCP KeepAlive。
// 对于内网环境尤其重要：防火墙/NAT/负载均衡器可能在连接空闲时
// 丢弃连接跟踪记录，导致长时间运行的脚本异常中断。
func setKeepAlive(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(tcpKeepAlive)
	}
}

// =============================================================================
// 连接处理
// =============================================================================

// HandleSshConn 处理一条 SSH 客户端连接：
//  1. 与客户端完成 SSH 握手
//  2. 从用户名解析目标地址、登录用户、密码
//  3. 与目标建立 SSH 连接
//  4. 双向转发 channel 与 global request
func (p *Proxy) HandleSshConn(ssc net.Conn, config *ssh.ServerConfig) {
	defer ssc.Close()

	// 启用 TCP KeepAlive，防止内网防火墙/NAT 在连接空闲时断开
	setKeepAlive(ssc)

	// ===== 与客户端 SSH 握手 =====
	cConn, cChans, cReqs, err := ssh.NewServerConn(ssc, config)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return // 客户端主动断开连接
		}
		slog.Error("failed to establish ssh proxy to client",
			"remote", ssc.RemoteAddr().String(), "err", err)
		return
	}
	defer cConn.Close()

	// ===== 解析登录信息 =====
	// 密码来源：用户名内嵌（含 ':'）或认证阶段客户端输入（Permissions.Extensions）
	passStr := ""
	if cConn.Permissions != nil {
		passStr = cConn.Permissions.Extensions[passKey]
	}
	target, err := parseTarget(cConn.User(), passStr, p.TargetAddr, ssc.RemoteAddr().String())
	if err != nil {
		slog.Error("parse target failed", "user", cConn.User(), "err", err)
		return
	}
	tTag := fmt.Sprintf("[%s: %s -> %s]", target.Name, ssc.RemoteAddr().String(), target.Addr)

	// ===== 连接目标（带超时，防止恶意目标阻塞） =====
	ttc, err := net.DialTimeout("tcp", target.Addr, dialTimeout)
	if err != nil {
		slog.Error("dial target failed", "tag", tTag, "err", err)
		return
	}
	defer ttc.Close()
	// 目标侧也要启用 KeepAlive，防止内网中间设备断开空闲连接
	setKeepAlive(ttc)

	slog.Info("sshp >>> begin", "tag", tTag, "user", target.User)
	defer slog.Info("sshp <<< final", "tag", tTag, "user", target.User)

	tConn, tChans, tReqs, err := ssh.NewClientConn(ttc, target.Addr, &ssh.ClientConfig{
		User:            target.User,
		Auth:            []ssh.AuthMethod{ssh.Password(target.Pass)},
		Timeout:         30 * time.Second,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		slog.Error("failed to establish ssh proxy to target", "tag", tTag, "err", err)
		return
	}
	defer tConn.Close()

	// ===== 双向转发 =====
	// 连接级桥接：global request 双向 + channel 双向 + 断开监控
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// global request 双向转发（两个独立 goroutine 保持并行）
	go p.ForwardRequest(cReqs, tConn) // client -> target
	go p.ForwardRequest(tReqs, cConn) // target -> client

	// channel 双向转发：两端各自独立监听，保持并行
	go p.ForwardChannel(cConn, tChans, tTag)
	go p.ForwardChannel(tConn, cChans, tTag)

	// 任一端断开时主动关闭对端并取消转发
	go func() { cConn.Wait(); tConn.Close(); cancel() }()
	go func() { tConn.Wait(); cConn.Close(); cancel() }()

	<-ctx.Done()
}

// =============================================================================
// 转发逻辑
// =============================================================================

// ForwardRequest 将全局请求从一端转发到另一端。
func (p *Proxy) ForwardRequest(reqs <-chan *ssh.Request, targetConn ssh.Conn) {
	for req := range reqs {
		result, payload, err := targetConn.SendRequest(req.Type, req.WantReply, req.Payload)
		if err != nil {
			continue
		}
		_ = req.Reply(result, payload)
	}
}

// ForwardChannel 循环监听 originChans，对每条新 channel 在 targetConn 上打开对应 channel 并双向转发。
// 随连接关闭（originChans 关闭）自然退出。
func (p *Proxy) ForwardChannel(targetConn ssh.Conn, originChans <-chan ssh.NewChannel, tTag string) {
	for originChannel := range originChans {
		go p.forwardChannel0(targetConn, originChannel, tTag)
	}
}

// forwardChannel 在两端之间转发一条 SSH channel。
//
// 共 6 个 goroutine：stdout×2 + stderr×2 + 请求×2，每个阻塞的 io.Copy / for range 独占 goroutine。
// 任一方向 stdout EOF 后 Close 对端 channel（发送 channelClose），触发对端 Read 返回，避免 wg.Wait 死锁。
func (p *Proxy) forwardChannel0(targetConn ssh.Conn, originChannel ssh.NewChannel, tTag string) {
	// 在目标端打开对应 channel
	targetChan, targetReqs, err := targetConn.OpenChannel(originChannel.ChannelType(), originChannel.ExtraData())
	if err != nil {
		slog.Error("open target channel error", "tag", tTag, "err", err)
		_ = originChannel.Reject(ssh.ConnectionFailed, "open target channel error")
		return
	}
	defer targetChan.Close()

	// 接受源端 channel
	originChan, originReqs, err := originChannel.Accept()
	if err != nil {
		slog.Error("accept origin channel failed", "tag", tTag, "err", err)
		return
	}
	defer originChan.Close()

	var wg sync.WaitGroup
	wg.Add(6)

	// origin -> target stdout
	// EOF 后立即 Close targetChan（发送 channelClose），让目标侧 Read 也返回，避免 wg.Wait 死锁。
	go func() {
		defer wg.Done()
		_, _ = io.Copy(targetChan, originChan)
		_ = targetChan.Close()
	}()
	// target -> origin stdout
	go func() {
		defer wg.Done()
		_, _ = io.Copy(originChan, targetChan)
		_ = originChan.Close()
	}()
	// origin -> target stderr
	go func() {
		defer wg.Done()
		_, _ = io.Copy(targetChan.Stderr(), originChan.Stderr())
	}()
	// target -> origin stderr
	go func() {
		defer wg.Done()
		_, _ = io.Copy(originChan.Stderr(), targetChan.Stderr())
	}()
	// channel 级请求双向转发（随 channel 关闭自然退出）
	forward := func(sourceReqs <-chan *ssh.Request, dst ssh.Channel) {
		defer wg.Done()
		for req := range sourceReqs {
			b, err := dst.SendRequest(req.Type, req.WantReply, req.Payload)
			_ = req.Reply(b, nil)
			if err != nil {
				return
			}
		}
	}
	go forward(originReqs, targetChan)
	go forward(targetReqs, originChan)

	wg.Wait()
}

// =============================================================================
// 目标地址解析
// =============================================================================

// targetInfo 保存从客户端用户名解析出的目标信息。
type targetInfo struct {
	Name string // 显示名（原始 server-name 部分）
	Addr string // 实际目标地址
	User string // 目标登录用户
	Pass string // 目标登录密码
}

// parseTarget 从 SSH 客户端用户名解析目标地址与用户，密码由外部传入。
//
// 用户名格式：server-name[/user-name][:password]
//   - 若用户名含 ':'，则冒号后为内嵌密码（优先使用）
//   - 若用户名不含 ':'，则使用 pass 参数（来自认证阶段客户端输入）
//
// server-name 支持：
//   - host                      -> {host}
//   - host-port                 -> {host},{port}
//   - host-port-ssvc{snum}      -> {host},{port},{ssvc},{snum}
//
// addrTpl 为目标地址模板，其中的 {host}/{port}/{ssvc}/{snum} 会被替换。
func parseTarget(cUser, pass, addrTpl, remote string) (*targetInfo, error) {
	if cUser == "" {
		return nil, fmt.Errorf("empty user name from %s", remote)
	}
	// 剥离可能内嵌的密码（含 ':' 时取冒号前作为主体）
	tPass := pass
	userMain := cUser
	if idx := strings.Index(cUser, ":"); idx >= 0 {
		tPass = cUser[idx+1:]
		userMain = cUser[:idx]
	}
	if tPass == "" {
		return nil, fmt.Errorf("empty password for user %q from %s", cUser, remote)
	}

	// 用户：server-name/user
	tUser := "user"
	tName := userMain
	if attr2 := strings.SplitN(userMain, "/", 2); len(attr2) == 2 {
		tName, tUser = attr2[0], attr2[1]
	}

	// 地址：host / host-port / host-port-ssvc{snum}
	tAddr := addrTpl
	parts := strings.SplitN(tName, "-", 3)
	tAddr = strings.ReplaceAll(tAddr, "{host}", parts[0])
	if len(parts) >= 2 {
		tAddr = strings.ReplaceAll(tAddr, "{port}", parts[1])
	}
	if len(parts) == 3 {
		// 从 ssvc{snum} 中切分出服务名与序号：首个数字字符为分界
		ssvc, snum := parts[2], ""
		if i := strings.IndexFunc(parts[2], func(c rune) bool { return c >= '0' && c <= '9' }); i >= 0 {
			ssvc, snum = parts[2][:i], parts[2][i:]
		}
		tAddr = strings.ReplaceAll(tAddr, "{ssvc}", ssvc)
		tAddr = strings.ReplaceAll(tAddr, "{snum}", snum)
	}

	return &targetInfo{Name: tName, Addr: tAddr, User: tUser, Pass: tPass}, nil
}

// =============================================================================
// 入口
// =============================================================================

func main() {
	p := &Proxy{
		KeysFolder: os.Getenv("KEYS_FOLDER"),
		ListenAddr: os.Getenv("LISTEN_ADDR"),
		TargetAddr: os.Getenv("TARGET_ADDR"),
	}
	if err := p.Start(); err != nil {
		slog.Error("sshp start failed", "err", err)
		os.Exit(1)
	}
	// InitConf 已在 Start 中完成，此时字段为实际生效值（含默认值）
	slog.Info("sshp is starting",
		"keys_folder", p.KeysFolder, "listen_addr", p.ListenAddr, "target_addr", p.TargetAddr)
	// 等待服务停止
	p.Wait()
	slog.Info("sshp is stopped")
}
