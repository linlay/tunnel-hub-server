package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	KindHTTP      = "http"
	KindWebSocket = "websocket"

	maxJSONFrameBytes = 1 << 20
)

type StreamRequest struct {
	Kind       string      `json:"kind"`
	RequestID  string      `json:"requestId"`
	Method     string      `json:"method"`
	Path       string      `json:"path"`
	Host       string      `json:"host"`
	Target     string      `json:"target"`
	Header     http.Header `json:"header"`
	BodyLength int64       `json:"bodyLength"`
}

type StreamResponse struct {
	OK         bool        `json:"ok"`
	StatusCode int         `json:"statusCode"`
	Header     http.Header `json:"header,omitempty"`
	BodyLength int64       `json:"bodyLength"`
	Error      string      `json:"error,omitempty"`
}

type WSFrameHeader struct {
	Type   int
	Length uint64
}

func WriteJSON(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxJSONFrameBytes {
		return fmt.Errorf("json frame too large: %d", len(data))
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(data)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func ReadJSON(r io.Reader, value any) error {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size > maxJSONFrameBytes {
		return fmt.Errorf("json frame too large: %d", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func WriteWSFrame(w io.Writer, messageType int, payload []byte) error {
	var header [9]byte
	header[0] = byte(messageType)
	binary.BigEndian.PutUint64(header[1:], uint64(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func ReadWSFrame(r io.Reader) (WSFrameHeader, []byte, error) {
	var raw [9]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return WSFrameHeader{}, nil, err
	}
	header := WSFrameHeader{Type: int(raw[0]), Length: binary.BigEndian.Uint64(raw[1:])}
	if header.Length > 64<<20 {
		return WSFrameHeader{}, nil, fmt.Errorf("websocket frame too large: %d", header.Length)
	}
	payload := make([]byte, header.Length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return WSFrameHeader{}, nil, err
	}
	return header, payload, nil
}

func CopyWebSocketToFrames(ws *websocket.Conn, dst io.Writer) error {
	for {
		messageType, payload, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if err := WriteWSFrame(dst, messageType, payload); err != nil {
			return err
		}
	}
}

func CopyFramesToWebSocket(src io.Reader, ws *websocket.Conn) error {
	for {
		header, payload, err := ReadWSFrame(src)
		if err != nil {
			return err
		}
		if err := ws.WriteMessage(header.Type, payload); err != nil {
			return err
		}
	}
}

func BuildTargetURL(target, path string, websocket bool) (string, error) {
	base, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("target must include scheme and host: %s", target)
	}
	requestPath, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	base.Path = joinURLPath(base.Path, requestPath.Path)
	base.RawQuery = requestPath.RawQuery
	if websocket {
		switch base.Scheme {
		case "http":
			base.Scheme = "ws"
		case "https":
			base.Scheme = "wss"
		case "ws", "wss":
		default:
			return "", fmt.Errorf("unsupported websocket target scheme: %s", base.Scheme)
		}
	}
	return base.String(), nil
}

func StripHopHeaders(header http.Header) http.Header {
	out := make(http.Header, len(header))
	for key, values := range header {
		if isHopHeader(key) {
			continue
		}
		for _, value := range values {
			out.Add(key, value)
		}
	}
	return out
}

func StripWebSocketDialHeaders(header http.Header) http.Header {
	out := StripHopHeaders(header)
	for _, key := range []string{
		"Sec-Websocket-Accept",
		"Sec-Websocket-Extensions",
		"Sec-Websocket-Key",
		"Sec-Websocket-Protocol",
		"Sec-Websocket-Version",
		"Sec-WebSocket-Accept",
		"Sec-WebSocket-Extensions",
		"Sec-WebSocket-Key",
		"Sec-WebSocket-Protocol",
		"Sec-WebSocket-Version",
	} {
		out.Del(key)
	}
	return out
}

func NormalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		return parsedHost
	}
	return strings.TrimSuffix(host, ".")
}

func joinURLPath(base, request string) string {
	if base == "" || base == "/" {
		if request == "" {
			return "/"
		}
		return request
	}
	if request == "" || request == "/" {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(request, "/")
}

func isHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

type WebSocketNetConn struct {
	conn    *websocket.Conn
	reader  io.Reader
	writeMu sync.Mutex
}

func NewWebSocketNetConn(conn *websocket.Conn) *WebSocketNetConn {
	return &WebSocketNetConn{conn: conn}
}

func (c *WebSocketNetConn) Read(p []byte) (int, error) {
	for c.reader == nil {
		messageType, reader, err := c.conn.NextReader()
		if err != nil {
			return 0, err
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		c.reader = reader
	}
	n, err := c.reader.Read(p)
	if errors.Is(err, io.EOF) {
		c.reader = nil
		if n > 0 {
			return n, nil
		}
		return c.Read(p)
	}
	return n, err
}

func (c *WebSocketNetConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	writer, err := c.conn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return 0, err
	}
	n, writeErr := writer.Write(p)
	closeErr := writer.Close()
	if writeErr != nil {
		return n, writeErr
	}
	return n, closeErr
}

func (c *WebSocketNetConn) Close() error {
	return c.conn.Close()
}

func (c *WebSocketNetConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *WebSocketNetConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *WebSocketNetConn) SetDeadline(t time.Time) error {
	if err := c.conn.SetReadDeadline(t); err != nil {
		return err
	}
	return c.conn.SetWriteDeadline(t)
}

func (c *WebSocketNetConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *WebSocketNetConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}
