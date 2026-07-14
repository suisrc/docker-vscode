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

// passKey 是 Permissions.Extensions 中存放客户端输入密码的 key。
const passKey = "pass"

// Proxy 表示一个 SSH 代理服务实例。
type Proxy struct {
	KeysFolder string
	ListenAddr string
	TargetAddr string
	Listener   net.Listener
	wchan      chan struct{}
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
		return errors.New("ssh proxy server is already running")
	}
	conf, err := p.InitConf()
	if err != nil {
		return err
	}
	p.Listener, err = net.Listen("tcp", p.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to start ssh proxy server: %s", err.Error())
	}
	if p.wchan != nil {
		close(p.wchan)
	}
	p.wchan = make(chan struct{})
	go func() {
		defer p.Listener.Close()
		slog.Info("ssh proxy server is listening on: " + p.ListenAddr)
		for {
			conn, err := p.Listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					slog.Info("ssh proxy server is closed")
					if p.wchan != nil {
						close(p.wchan)
						p.wchan = nil
					}
					return
				}
				slog.Error("failed to accept incoming connection: " + err.Error())
				continue
			}
			go p.HandleSshConn(conn, conf)
		}
	}()
	return nil
}

// InitConf 初始化 SSH 服务端配置与默认参数。
//
// 双认证模式：
//   - 用户名内嵌密码（含 ':'）：NoClientAuthCallback 直接放行
//   - 用户名无密码（不含 ':'）：NoClientAuthCallback 返回 PartialSuccessError，
//     要求客户端继续密码认证，由 PasswordCallback 收集密码
func (p *Proxy) InitConf() (*ssh.ServerConfig, error) {
	passwordCb := func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if strings.Contains(meta.User(), ":") {
			return nil, errors.New("password already embedded in username")
		}
		return &ssh.Permissions{Extensions: map[string]string{passKey: string(password)}}, nil
	}
	conf := &ssh.ServerConfig{
		NoClientAuth: true,
		NoClientAuthCallback: func(meta ssh.ConnMetadata) (*ssh.Permissions, error) {
			if strings.Contains(meta.User(), ":") {
				return nil, nil
			}
			return nil, &ssh.PartialSuccessError{
				Next: ssh.ServerAuthCallbacks{
					PasswordCallback: passwordCb,
				},
			}
		},
		PasswordCallback: passwordCb,
	}
	if p.KeysFolder == "" {
		p.KeysFolder = "/etc/ssh"
	}
	for _, name := range []string{"ssh_host_rsa_key", "ssh_host_ecdsa_key", "ssh_host_ed25519_key"} {
		if err := p.AddHostKey(conf, p.KeysFolder+"/"+name); err != nil {
			return nil, err
		}
	}
	if p.ListenAddr == "" {
		p.ListenAddr = ":22"
	}
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

// HandleSshConn 处理一条 SSH 客户端连接：
//  1. 与客户端完成 SSH 握手
//  2. 从用户名解析目标地址、登录用户、密码
//  3. 与目标建立 SSH 连接
//  4. 双向转发 channel 与 global request
func (p *Proxy) HandleSshConn(ssc net.Conn, config *ssh.ServerConfig) {
	defer ssc.Close()
	// SSH 握手
	cConn, cChans, cReqs, err := ssh.NewServerConn(ssc, config)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return // 客户端主动断开连接
		}
		slog.Error("failed to establish ssh proxy to client: " +
			ssc.RemoteAddr().String() + " -> " + err.Error())
		return
	}
	defer cConn.Close()

	// ===== 解析登录信息 =====
	cUser := cConn.User()
	if cUser == "" {
		slog.Error("failed to get user name from ssh connection " + ssc.RemoteAddr().String())
		return
	}

	// 密码：内嵌（含 ':'）或后输入（Permissions.Extensions）
	tPass := ""
	if idx := strings.Index(cUser, ":"); idx >= 0 {
		tPass = cUser[idx+1:]
		cUser = cUser[:idx]
	} else if cConn.Permissions != nil {
		tPass = cConn.Permissions.Extensions[passKey]
	}
	if tPass == "" {
		slog.Error("invalid user name or empty password: " + cUser)
		return
	}

	// 用户
	tUser := "user"
	tName := cUser
	if attr2 := strings.SplitN(cUser, "/", 2); len(attr2) == 2 {
		tName, tUser = attr2[0], attr2[1]
	}

	// 地址：host / host-port / host-port-ssvc{snum}
	tAddr := p.TargetAddr
	attr3 := strings.SplitN(tName, "-", 3)
	if len(attr3) == 3 {
		attr3 = append(attr3, "")
		for i := 0; i < len(attr3[2]); i++ {
			if attr3[2][i] < '0' || attr3[2][i] > '9' {
				continue
			}
			attr3[3] = attr3[2][i:] // snum
			attr3[2] = attr3[2][:i] // ssvc
			break
		}
		tAddr = strings.ReplaceAll(tAddr, "{host}", attr3[0])
		tAddr = strings.ReplaceAll(tAddr, "{port}", attr3[1])
		tAddr = strings.ReplaceAll(tAddr, "{ssvc}", attr3[2])
		tAddr = strings.ReplaceAll(tAddr, "{snum}", attr3[3])
	} else if len(attr3) == 2 {
		tAddr = strings.ReplaceAll(tAddr, "{host}", attr3[0])
		tAddr = strings.ReplaceAll(tAddr, "{port}", attr3[1])
	} else {
		tAddr = strings.ReplaceAll(tAddr, "{host}", tName)
	}

	tTag := fmt.Sprintf("[%s: %s -> %s]", tName, ssc.RemoteAddr().String(), tAddr)

	// ===== 连接目标 =====
	ttc, err := net.Dial("tcp", tAddr)
	if err != nil {
		slog.Error(tTag + " dial failed: " + err.Error())
		return
	}
	defer ttc.Close()

	slog.Info(tTag + " ssh proxy >>> begin, username: " + tUser)
	defer slog.Info(tTag + " ssh proxy <<< final, username: " + tUser)

	tConn, tChans, tReqs, err := ssh.NewClientConn(ttc, tAddr, &ssh.ClientConfig{
		User:            tUser,
		Auth:            []ssh.AuthMethod{ssh.Password(tPass)},
		Timeout:         30 * time.Second,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		slog.Error(tTag + " failed to establish ssh proxy to target: " + err.Error())
		return
	}
	defer tConn.Close()

	// ===== 双向转发 =====
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.ForwardRequest(cReqs, tConn, tTag) // client -> target
	go p.ForwardRequest(tReqs, cConn, tTag) // target -> client

	go func() {
		for nChan := range tChans {
			go p.ForwardChannel(ctx, cConn, nChan, tTag)
		}
	}()
	go func() {
		for nChan := range cChans {
			go p.ForwardChannel(ctx, tConn, nChan, tTag)
		}
	}()

	go func() { cConn.Wait(); cancel() }()
	go func() { tConn.Wait(); cancel() }()

	<-ctx.Done()
}

// ForwardRequest 将全局请求从一端转发到另一端。
func (p *Proxy) ForwardRequest(reqs <-chan *ssh.Request, targetConn ssh.Conn, tTag string) {
	for req := range reqs {
		result, payload, err := targetConn.SendRequest(req.Type, req.WantReply, req.Payload)
		if err != nil {
			continue
		}
		_ = req.Reply(result, payload)
	}
}

// ForwardChannel 在两端之间转发一条 SSH channel。
func (p *Proxy) ForwardChannel(ctx context.Context, targetConn ssh.Conn, originChannel ssh.NewChannel, tTag string) {
	targetChan, targetReqs, err := targetConn.OpenChannel(originChannel.ChannelType(), originChannel.ExtraData())
	if err != nil {
		slog.Error(tTag + " open target channel error: " + err.Error())
		_ = originChannel.Reject(ssh.ConnectionFailed, "open target channel error")
		return
	}
	defer targetChan.Close()

	originChan, originReqs, err := originChannel.Accept()
	if err != nil {
		slog.Error(tTag + " accept origin channel failed: " + err.Error())
		return
	}
	defer originChan.Close()

	maskedReqs := make(chan *ssh.Request, 1)
	go func() {
		for req := range originReqs {
			maskedReqs <- req
		}
		close(maskedReqs)
	}()

	originWg := sync.WaitGroup{}
	originWg.Add(3)
	targetWg := sync.WaitGroup{}
	targetWg.Add(3)

	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(targetChan, originChan)
		_ = targetChan.CloseWrite()
		targetWg.Done()
		targetWg.Wait()
		_ = targetChan.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(originChan, targetChan)
		_ = originChan.CloseWrite()
		originWg.Done()
		originWg.Wait()
		_ = originChan.Close()
	}()
	go func() {
		_, _ = io.Copy(targetChan.Stderr(), originChan.Stderr())
		targetWg.Done()
	}()
	go func() {
		_, _ = io.Copy(originChan.Stderr(), targetChan.Stderr())
		originWg.Done()
	}()

	forward := func(sourceReqs <-chan *ssh.Request, targetChan ssh.Channel, channelWg *sync.WaitGroup) {
		defer channelWg.Done()
		for ctx.Err() == nil {
			select {
			case req, ok := <-sourceReqs:
				if !ok {
					return
				}
				b, err := targetChan.SendRequest(req.Type, req.WantReply, req.Payload)
				_ = req.Reply(b, nil)
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}

	go forward(maskedReqs, targetChan, &targetWg)
	go forward(targetReqs, originChan, &originWg)

	wg.Wait()
}

func main() {
	p := &Proxy{
		KeysFolder: os.Getenv("KEYS_FOLDER"),
		ListenAddr: os.Getenv("LISTEN_ADDR"),
		TargetAddr: os.Getenv("TARGET_ADDR"),
	}
	slog.Info("ssh proxy server is starting...",
		"keys_folder", p.KeysFolder, "listen_addr", p.ListenAddr, "target_addr", p.TargetAddr)
	if err := p.Start(); err != nil {
		slog.Error("ssh proxy server start failed: " + err.Error())
		os.Exit(1)
	}
	p.Wait()
	slog.Info("ssh proxy server is stopped")
}
