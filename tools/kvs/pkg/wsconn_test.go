package pkg

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"testing"
	"time"
)

// TestWSUnmaskMatchesNaive 校验字长化 unmask 与逐字节实现在任意长度/掩码下一致。
func TestWSUnmaskMatchesNaive(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for _, n := range []int{0, 1, 3, 4, 7, 8, 9, 15, 16, 17, 63, 64, 65, 1000, 4096, 4097} {
		payload := make([]byte, n)
		rnd.Read(payload)
		var key [4]byte
		rnd.Read(key[:])
		got := append([]byte(nil), payload...)
		want := append([]byte(nil), payload...)
		wsUnmask(got, key)
		for i := range want {
			want[i] ^= key[i&3]
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("n=%d: unmask mismatch\n got=%x\nwant=%x", n, got, want)
		}
	}
}

// TestWSFrameRoundTrip 覆盖新的帧读写路径：小帧（合并写/读缓冲复用）、大帧
// （writev + 独立分配）、以及掩码/未掩码校验。
func TestWSFrameRoundTrip(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	ws := &wsConn{conn: srv, br: bufio.NewReaderSize(srv, wsReadBufSize)}

	sizes := []int{0, 1, 125, 126, 65535, 65536, 200000, wsReadReuseMax + 1}
	for _, size := range sizes {
		payload := bytes.Repeat([]byte{byte(size)}, size)

		// 客户端 → 服务端（掩码帧）
		go func(p []byte) {
			var buf bytes.Buffer
			writeClientFrame(&buf, wsOpText, p)
			_, _ = cli.Write(buf.Bytes())
		}(payload)
		got, op, err := ws.wsReadMessage()
		if err != nil {
			t.Fatalf("size=%d: read: %v", size, err)
		}
		if op != wsOpText || !bytes.Equal(got, payload) {
			t.Fatalf("size=%d: payload mismatch (op=%v, len=%d)", size, op, len(got))
		}

		// 服务端 → 客户端（非掩码帧）
		go func(p []byte) {
			if err := ws.wsWriteText(p); err != nil {
				t.Errorf("size=%d: write: %v", size, err)
			}
		}(payload)
		op2, back, err := readClientFrame(cli)
		if err != nil {
			t.Fatalf("size=%d: client read: %v", size, err)
		}
		if op2 != wsOpText || !bytes.Equal(back, payload) {
			t.Fatalf("size=%d: client payload mismatch (op=%v, len=%d)", size, op2, len(back))
		}
	}
}

// TestWSReadReuseContract 固化读缓冲复用契约：小帧复用同一块内存，超过上限的帧
// 单独分配（避免每条连接常驻大缓冲）。
func TestWSReadReuseContract(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	ws := &wsConn{conn: srv, br: bufio.NewReader(srv)}

	// net.Pipe 是同步的：写入必须放在 goroutine 里，否则会在 peer 读取前死锁。
	write := func(p []byte) {
		var buf bytes.Buffer
		writeClientFrame(&buf, wsOpText, p)
		frame := buf.Bytes()
		go func() { _, _ = cli.Write(frame) }()
	}

	small := bytes.Repeat([]byte("a"), 128)
	write(small)
	first, _, err := ws.wsReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	firstPtr := &first[0]
	write(small)
	second, _, err := ws.wsReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if &second[0] != firstPtr {
		t.Errorf("小帧应复用同一读缓冲：%p != %p", &second[0], firstPtr)
	}

	big := bytes.Repeat([]byte("b"), wsReadReuseMax+16)
	write(big)
	third, _, err := ws.wsReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if &third[0] == firstPtr {
		t.Errorf("超过复用上限的帧应单独分配")
	}
}

// TestWSPingPong 校验控制帧路径（ping 经 writeFrame 回 pong），并且 ping 之后
// 继续读取下一帧（ping 不结束一次 wsReadMessage）。
func TestWSPingPong(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	ws := &wsConn{conn: srv, br: bufio.NewReader(srv)}

	// 客户端一次写入 ping + text 两帧（ping 由服务端内联应答）
	var buf bytes.Buffer
	writeClientFrame(&buf, wsOpPing, []byte("hb"))
	writeClientFrame(&buf, wsOpText, []byte("after-ping"))
	raw := buf.Bytes()
	go func() { _, _ = cli.Write(raw) }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 客户端只会收到 pong（text 帧是从客户端发出的，不会回显回来）。
		op, payload, err := readClientFrame(cli)
		if err != nil {
			t.Errorf("pong read: %v", err)
			return
		}
		if op != wsOpPong || string(payload) != "hb" {
			t.Errorf("expected pong hb, got op=%v payload=%q", op, payload)
		}
	}()

	got, op, err := ws.wsReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != wsOpText || string(got) != "after-ping" {
		t.Fatalf("wsReadMessage returned op=%v payload=%q, want text/after-ping", op, got)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for pong")
	}
}

// TestWSWriteDeadlineRefresh 粗粒度写超时：连续写不应反复设置 deadline，但长时间
// 闲置后必须重新设置（否则会误杀长寿连接）。
func TestWSWriteDeadlineRefresh(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	ws := &wsConn{conn: srv, br: bufio.NewReader(srv)}
	go func() { io.Copy(io.Discard, cli) }() // 消费，避免 net.Pipe 阻塞
	if err := ws.wsWriteText([]byte("x")); err != nil {
		t.Fatal(err)
	}
	first := ws.dlSet
	if first.IsZero() {
		t.Fatal("首次写必须设置写超时")
	}
	if err := ws.wsWriteText([]byte("y")); err != nil {
		t.Fatal(err)
	}
	if !ws.dlSet.Equal(first) {
		t.Errorf("短时间内不该重复设置写超时")
	}
	ws.dlSet = time.Now().Add(-wsWriteTimeout) // 模拟"上次设置在很久之前"
	if err := ws.wsWriteText([]byte("z")); err != nil {
		t.Fatal(err)
	}
	if ws.dlSet.Equal(first) {
		t.Errorf("距上次设置超过一半时必须重新设置写超时")
	}
}

// --- 测试辅助 ---

// writeClientFrame 写一个客户端→服务端的掩码帧。
func writeClientFrame(buf *bytes.Buffer, op wsOpcode, payload []byte) {
	var hdr [14]byte
	n := len(payload)
	var hn int
	switch {
	case n < 126:
		hdr[0], hdr[1] = 0x80|byte(op), 0x80|byte(n)
		hn = 2
	case n <= 0xFFFF:
		hdr[0], hdr[1] = 0x80|byte(op), 0x80|126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hn = 4
	default:
		hdr[0], hdr[1] = 0x80|byte(op), 0x80|127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hn = 10
	}
	var key [4]byte
	copy(hdr[hn:hn+4], key[:]) // 掩码 0：载荷不变，便于断言
	hn += 4
	buf.Write(hdr[:hn])
	if n > 0 {
		buf.Write(payload)
	}
}

// readClientFrame 读一个服务端→客户端的非掩码帧。
func readClientFrame(r io.Reader) (wsOpcode, []byte, error) {
	br := bufio.NewReader(r)
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return 0, nil, err
	}
	op := wsOpcode(hdr[0] & 0x0F)
	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if hdr[1]&0x80 != 0 {
		return 0, nil, errClientMasked
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}

var errClientMasked = errTest("server frame must not be masked")

type errTest string

func (e errTest) Error() string { return string(e) }
