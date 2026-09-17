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
// one reader goroutine per connection and serializes writes through the
// relay's mutex.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	// writeMu guards frame writes so reader (pongs) and relay sends cannot
	// interleave half-frames.
	writeMu chan struct{}
	closed  bool
}

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
	return &wsConn{
		conn:    netConn,
		br:      brw.Reader,
		writeMu: make(chan struct{}, 1),
	}, nil
}

// wsReadMessage reads one complete message. Only unfragmented text/binary
// frames are supported (the zcode clients send one JSON message per frame).
// Ping is answered inline; close completes the close handshake and returns
// wsErrClosed. Control frames may interleave legally and are handled here.
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
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i&3]
		}
	} else {
		// RFC 6455 §5.1: client-to-server frames MUST be masked.
		err = fmt.Errorf("unmasked client frame")
	}
	return
}

// writeFrame writes one frame with the FIN bit set. Server frames are never
// masked.
func (c *wsConn) writeFrame(op wsOpcode, payload []byte) error {
	c.writeMu <- struct{}{}
	defer func() { <-c.writeMu }()
	if c.closed {
		return wsErrClosed
	}
	var hdr []byte
	n := len(payload)
	switch {
	case n < 126:
		hdr = []byte{byte(0x80 | op), byte(n)}
	case n <= 0xFFFF:
		hdr = make([]byte, 4)
		hdr[0], hdr[1] = byte(0x80|op), 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
	default:
		hdr = make([]byte, 10)
		hdr[0], hdr[1] = byte(0x80|op), 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	if _, err := c.conn.Write(hdr); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.conn.Write(payload); err != nil {
			return err
		}
	}
	return c.conn.SetWriteDeadline(time.Time{})
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
	c.writeMu <- struct{}{}
	if !c.closed {
		c.closed = true
		_ = c.conn.Close()
	}
	<-c.writeMu
}
