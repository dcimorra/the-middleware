package browsers

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

// The server half of RFC 6455, as much of it as a DevTools client uses:
// text and binary messages, fragments, ping, pong and close. The core
// takes no dependency (AGENTS.md), and the house is the server here, so
// there is no client half: a client's frames are masked and ours are not.

const (
	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xA
)

// maxMessage bounds one message from a client. A full-page screenshot
// comes the other way; what a client sends is commands, and an
// Runtime.evaluate of a large script is still far below this.
const maxMessage = 64 << 20

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsConn is one client's connection.
type wsConn struct {
	c  net.Conn
	br *bufio.Reader
	wm sync.Mutex
	// closed is set once a close frame was sent, so a second one is not.
	closed bool
}

// isUpgrade says whether r asks for a websocket.
func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && headerHas(r.Header, "Connection", "upgrade")
}

func headerHas(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// upgrade answers the handshake and takes the connection over.
func upgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != http.MethodGet || !isUpgrade(r) || key == "" {
		http.Error(w, "a websocket is expected here", http.StatusBadRequest)
		return nil, errors.New("not a websocket handshake")
	}
	if v := r.Header.Get("Sec-WebSocket-Version"); v != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "websocket version 13 is expected", http.StatusUpgradeRequired)
		return nil, fmt.Errorf("websocket version %q", v)
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "this server cannot hand the connection over", http.StatusInternalServerError)
		return nil, errors.New("no hijacker")
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	_ = c.SetDeadline(time.Time{})
	if _, err := c.Write([]byte(resp)); err != nil {
		c.Close()
		return nil, err
	}
	return &wsConn{c: c, br: brw.Reader}, nil
}

// read returns the next whole data message, answering pings on the way.
// io.EOF is a close, from the client or from the connection.
func (ws *wsConn) read() ([]byte, error) {
	var msg []byte
	started := false
	for {
		fin, op, payload, err := ws.frame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := ws.write(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = ws.write(opClose, payload)
			return nil, io.EOF
		case opText, opBinary:
			if started {
				return nil, errors.New("websocket: a new message inside a fragmented one")
			}
			started = true
			msg = payload
		case opCont:
			if !started {
				return nil, errors.New("websocket: a continuation with nothing to continue")
			}
			if len(msg)+len(payload) > maxMessage {
				return nil, errors.New("websocket: message too large")
			}
			msg = append(msg, payload...)
		default:
			return nil, fmt.Errorf("websocket: opcode %d", op)
		}
		if fin {
			return msg, nil
		}
	}
}

// frame reads one frame and unmasks it.
func (ws *wsConn) frame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(ws.br, h[:]); err != nil {
		return false, 0, nil, eof(err)
	}
	fin = h[0]&0x80 != 0
	op = h[0] & 0x0F
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: reserved bits set (no extension was agreed)")
	}
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(ws.br, b[:]); err != nil {
			return false, 0, nil, eof(err)
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(ws.br, b[:]); err != nil {
			return false, 0, nil, eof(err)
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= opClose && (n > 125 || !fin) {
		return false, 0, nil, errors.New("websocket: a control frame too large or fragmented")
	}
	if n > maxMessage {
		return false, 0, nil, errors.New("websocket: message too large")
	}
	if !masked {
		// RFC 6455 5.1: a server closes on an unmasked client frame.
		return false, 0, nil, errors.New("websocket: a client frame without a mask")
	}
	var mask [4]byte
	if _, err = io.ReadFull(ws.br, mask[:]); err != nil {
		return false, 0, nil, eof(err)
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(ws.br, payload); err != nil {
		return false, 0, nil, eof(err)
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, op, payload, nil
}

func eof(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return io.EOF
	}
	return err
}

// write sends one unfragmented frame.
func (ws *wsConn) write(op byte, payload []byte) error {
	ws.wm.Lock()
	defer ws.wm.Unlock()
	if ws.closed {
		return net.ErrClosed
	}
	if op == opClose {
		ws.closed = true
	}
	head := make([]byte, 0, 10)
	head = append(head, 0x80|op)
	switch n := len(payload); {
	case n < 126:
		head = append(head, byte(n))
	case n <= 0xFFFF:
		head = append(head, 126, byte(n>>8), byte(n))
	default:
		head = append(head, 127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	// A client that stopped reading must not hold the browser's output:
	// the relay writes to one client from the browser's single reader.
	_ = ws.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := ws.c.Write(head); err != nil {
		return err
	}
	_, err := ws.c.Write(payload)
	return err
}

// text sends a text message.
func (ws *wsConn) text(b []byte) error { return ws.write(opText, b) }

// shut sends a close frame with code and reason, then closes.
func (ws *wsConn) shut(code uint16, reason string) {
	p := binary.BigEndian.AppendUint16(nil, code)
	if len(reason) > 120 {
		reason = reason[:120]
	}
	p = append(p, reason...)
	_ = ws.write(opClose, p)
	_ = ws.c.Close()
}
