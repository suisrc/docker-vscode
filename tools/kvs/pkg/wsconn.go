package pkg

// Minimal RFC 6455 WebSocket server endpoint — stdlib only, no third-party
// dependencies. Implements exactly the subset the zcode relay needs:
//
//   - server-side handshake (101 Switching Protocols, Sec-WebSocket-Accept)
//   - text frames as whole messages (no fragmentation support: a continuation
//     frame without a preceding fragment aborts the connection; the official
//     zcode clients never fragment)
//   - ping → pong, close handshake, masked client frames (RFC requires
//     servers to unmask; servers send unmasked)
//   - NO permessage-deflate: the extension is simply not negotiated, so
//     standard clients fall back to uncompressed frames automatically
//
// Not implemented (on purpose): client mode, extensions, fragmentation,
// multiplexing. Anything unexpected terminates the connection — the relay
// state machine treats a dead socket as a detach.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wsMagicGUID is the fixed GUID from RFC 6455 §1.3.
const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsMaxFrameBytes caps one inbound message. The zcode clients enforce 8MB
// physical frames; 4x headroom mirrors the reference relay's maxPayload.
const wsMaxFrameBytes = 8 * 1024 * 1024 * 4

// wsErrClosed is returned once the peer initiated (or we sent) a close.
var wsErrClosed = errors.New("websocket: connection closed")

// wsOpcode is an RFC 6455 frame opcode.
type wsOpcode byte

const (
	wsOpContinuation wsOpcode = 0x0
	wsOpText         wsOpcode = 0x1
	wsOpBinary       wsOpcode = 0x2
	wsOpClose        wsOpcode = 0x8
	wsOpPing         wsOpcode = 0x9
	wsOpPong         wsOpcode = 0xA
)

// wsConn is a hijacked, upgraded connection speaking minimal RFC 6455.
// It is NOT safe for concurrent use by multiple goroutines; the relay runs
// one reader goroutine per connection and serializes writes through writeMu.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	// writeMu guards frame writes so reader (pongs) and relay sends cannot
	// interleave half-frames. A plain mutex beats the previous buffered
	// channel: no scheduler handoff on the uncontended path.
	writeMu sync.Mutex
	// wbuf is scratch space for small frames, so header+payload leave in one
	// write (guarded by writeMu). Frames larger than wbuf use writev instead.
	wbuf [wsSmallFrameBytes]byte
	// dlSet is when the write deadline was last pushed out; see writeFrame.
	dlSet time.Time
	// rbuf is the reusable read buffer (see wsReadMessage for the lifetime
	// contract). Frames larger than wsReadReuseMax allocate per call.
	rbuf   []byte
	closed bool
}

// 帧 I/O 参数。
const (
	// wsSmallFrameBytes: 小于该值的帧合并到一块缓冲里一次写出（TLS 连接没有
	// writev，合并写避免"头一个 TLS 记录 + 载荷一个记录"）。
	wsSmallFrameBytes = 4 << 10
	// wsReadReuseMax: 读缓冲复用上限，避免每帧 make 造成 GC 压力；更大的帧
	// （少见）按需分配，不用为了极端帧让每条连接常驻几十 MB。
	wsReadReuseMax = 1 << 20
	// wsReadBufSize: hijack 后重新包装读缓冲的大小（server 默认只有 4KB）。
	wsReadBufSize = 64 << 10
	// wsWriteTimeout: 单次写的超时；按"过半才刷新"的粗粒度设置 deadline，
	// 不再每帧两次 SetWriteDeadline。
	wsWriteTimeout = 30 * time.Second
)

// wsUpgrade hijacks the connection and completes the server-side WebSocket
// handshake. Returns nil (and answers 400) when the request is not a valid
// upgrade. Extensions (permessage-deflate) are silently ignored — no
// extension is negotiated, so both sides use plain frames.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !isWebSocketUpgrade(r) {
		http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
		return nil, fmt.Errorf("not a websocket upgrade")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be hijacked", http.StatusInternalServerError)
		return nil, fmt.Errorf("response writer cannot hijack")
	}
	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijack: %w", err)
	}

	// Compute Sec-WebSocket-Accept = base64(sha1(key + GUID)).
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsMagicGUID))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n" +
		"\r\n"
	if _, err := netConn.Write([]byte(resp)); err != nil {
		netConn.Close()
		return nil, fmt.Errorf("write handshake: %w", err)
	}
	// The hijacked bufio.Reader is 4KB. When it holds nothing (the common case:
	// the client waited for the 101) it can be replaced by a larger one, which
	// cuts a read syscall for every medium frame. When it does hold bytes they
	// must be preserved, so the original reader stays.
	br := brw.Reader
	if br.Buffered() == 0 {
		br = bufio.NewReaderSize(netConn, wsReadBufSize)
	}
	return &wsConn{
		conn: netConn,
		br:   br,
	}, nil
}

// wsReadMessage reads one complete message. Only unfragmented text/binary
// frames are supported (the zcode clients send one JSON message per frame).
// Ping is answered inline; close completes the close handshake and returns
// wsErrClosed. Control frames may interleave legally and are handled here.
//
// LIFETIME: the returned payload may alias the connection's reusable read
// buffer (frames ≤ wsReadReuseMax). It is valid only until the next
// wsReadMessage call on the same connection — the relay uses it synchronously
// (forward it, parse it) before reading again, which is exactly the contract.
func (c *wsConn) wsReadMessage() ([]byte, wsOpcode, error) {
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, 0, err
		}
		switch op {
		case wsOpPing:
			_ = c.writeFrame(wsOpPong, payload)
			continue
		case wsOpPong:
			continue // unsolicited pongs are ignored
		case wsOpClose:
			// Echo the close and tear down (best effort).
			_ = c.writeFrame(wsOpClose, payload)
			c.close()
			return nil, 0, wsErrClosed
		case wsOpContinuation:
			// A continuation without a fragment start is a protocol error.
			c.Abort()
			return nil, 0, fmt.Errorf("unexpected continuation frame")
		case wsOpText, wsOpBinary:
			if !fin {
				// Fragmented message: not supported by the relay protocol.
				c.Abort()
				return nil, 0, fmt.Errorf("fragmented message not supported")
			}
			return payload, op, nil
		default:
			c.Abort()
			return nil, 0, fmt.Errorf("unknown opcode 0x%x", op)
		}
	}
}

// readFrame reads a single RFC 6455 frame header + payload. Server side:
// client frames MUST be masked and are unmasked here; our own frames are
// sent unmasked per RFC 6455 §5.1.
func (c *wsConn) readFrame() (fin bool, op wsOpcode, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.br, hdr[:]); err != nil {
		return
	}
	fin = hdr[0]&0x80 != 0
	rsv := hdr[0] & 0x70 // RSV bits must be zero (no extensions negotiated)
	op = wsOpcode(hdr[0] & 0x0F)
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > wsMaxFrameBytes {
		err = fmt.Errorf("frame too large: %d", length)
		return
	}
	if rsv != 0 {
		err = fmt.Errorf("RSV bits set (extension negotiated unexpectedly)")
		return
	}
	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, maskKey[:]); err != nil {
			return
		}
	}
	// 复用连接级读缓冲：稳态下每帧零分配（> wsReadReuseMax 的帧才单独分配）。
	if length <= wsReadReuseMax {
		if cap(c.rbuf) < int(length) {
			c.rbuf = make([]byte, length)
		}
		payload = c.rbuf[:length]
	} else {
		payload = make([]byte, length)
	}
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		wsUnmask(payload, maskKey)
	} else {
		// RFC 6455 §5.1: client-to-server frames MUST be masked.
		err = fmt.Errorf("unmasked client frame")
	}
	return
}

// wsUnmask XORs payload with the 4-byte frame mask key. Word-sized XOR for the
// bulk of the payload (the byte loop inlined by the compiler is ~1 byte/cycle;
// this is ~8), byte-wise for the tail.
func wsUnmask(payload []byte, key [4]byte) {
	k := uint64(binary.NativeEndian.Uint32(key[:]))
	mk := k | k<<32
	i := 0
	for ; i+8 <= len(payload); i += 8 {
		v := binary.NativeEndian.Uint64(payload[i:])
		binary.NativeEndian.PutUint64(payload[i:], v^mk)
	}
	for ; i < len(payload); i++ {
		payload[i] ^= key[i&3]
	}
}

// writeFrame writes one frame with the FIN bit set. Server frames are never
// masked. Header and payload leave in a single syscall:小帧合并进 wbuf 一次
// Write，大帧用 net.Buffers → writev（TLS 等不支持 writev 的实现退化为两次
// 顺序写）。写超时按"过半才刷新"维护，稳态下每帧不再调用 SetWriteDeadline。
func (c *wsConn) writeFrame(op wsOpcode, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return wsErrClosed
	}
	var hdr [10]byte
	n := len(payload)
	var hn int
	switch {
	case n < 126:
		hdr[0], hdr[1] = byte(0x80|op), byte(n)
		hn = 2
	case n <= 0xFFFF:
		hdr[0], hdr[1] = byte(0x80|op), 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hn = 4
	default:
		hdr[0], hdr[1] = byte(0x80|op), 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hn = 10
	}
	// 只在剩余不足一半时才推写超时（等价语义，稳态零额外调用）。
	if c.dlSet.IsZero() || time.Since(c.dlSet) >= wsWriteTimeout/2 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
			return err
		}
		c.dlSet = time.Now()
	}
	if hn+n <= len(c.wbuf) {
		buf := c.wbuf[:0]
		buf = append(buf, hdr[:hn]...)
		buf = append(buf, payload...)
		_, err := c.conn.Write(buf)
		return err
	}
	bufs := net.Buffers{hdr[:hn]} // 局部切片：WriteTo 会推进它，不动底层数组
	if n > 0 {
		bufs = append(bufs, payload)
	}
	_, err := bufs.WriteTo(c.conn)
	return err
}

// wsWriteText sends one text message (single frame, FIN set).
func (c *wsConn) wsWriteText(payload []byte) error {
	return c.writeFrame(wsOpText, payload)
}

// Close sends a proper close frame then closes the TCP connection.
func (c *wsConn) Close(reason string) {
	if len(reason) > 123 {
		reason = reason[:123] // close payload limit: 125 - 2 (status code)
	}
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, 1000) // normal closure
	copy(payload[2:], reason)
	_ = c.writeFrame(wsOpClose, payload)
	c.close()
}

// Abort drops the TCP connection without a close handshake (protocol error).
func (c *wsConn) Abort() { c.close() }

// close tears down the TCP connection exactly once.
func (c *wsConn) close() {
	c.writeMu.Lock()
	if !c.closed {
		c.closed = true
		_ = c.conn.Close()
	}
	c.writeMu.Unlock()
}
