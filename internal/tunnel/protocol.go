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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	KindHTTP      = "http"
	KindWebSocket = "websocket"

	ProtocolVersion = 1

	NamespaceDesktop = "d"
	NamespaceWebApp  = "wa"

	FrameRequest  = "request"
	FrameResponse = "response"
	FrameError    = "error"

	TypeTunnelOpen           = "tunnel.open"
	TypeDesktopWebSocket     = "websocket"
	TypeDesktopWebSocketOpen = "desktop.websocket.open"
	TypeWebAppHTTPRequest    = "http.request"
	TypeWebAppHTTPResponse   = "http.response"
	TypeWebSocketConnect     = "websocket.connect"
	TypeWebSocketAccept      = "websocket.accept"
	TypeError                = "error"

	maxJSONFrameBytes = 1 << 20
)

type PublicRequest struct {
	Method  string      `json:"method"`
	Host    string      `json:"host"`
	Path    string      `json:"path"`
	Headers http.Header `json:"headers,omitempty"`
}

type UpstreamTarget struct {
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	BasePath string `json:"basePath"`
}

type RouteMetadata struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	PublicHost string `json:"publicHost,omitempty"`
}

type StreamPayload struct {
	AgentToken     string          `json:"agentToken,omitempty"`
	DeviceID       string          `json:"deviceId,omitempty"`
	Client         string          `json:"client,omitempty"`
	Capabilities   []string        `json:"capabilities,omitempty"`
	AuthToken      string          `json:"authToken,omitempty"`
	Subprotocol    string          `json:"subprotocol,omitempty"`
	Source         string          `json:"source,omitempty"`
	ClientDeviceID string          `json:"clientDeviceId,omitempty"`
	Public         *PublicRequest  `json:"public,omitempty"`
	Upstream       *UpstreamTarget `json:"upstream,omitempty"`
	Route          *RouteMetadata  `json:"route,omitempty"`
	BodyLength     *int64          `json:"bodyLength,omitempty"`
}

type StreamResponseData struct {
	SessionID  string      `json:"sessionId,omitempty"`
	Multiplex  string      `json:"multiplex,omitempty"`
	StatusCode int         `json:"statusCode,omitempty"`
	Headers    http.Header `json:"headers,omitempty"`
	BodyLength *int64      `json:"bodyLength,omitempty"`
}

type StreamRequest struct {
	V          int             `json:"v,omitempty"`
	NS         string          `json:"ns,omitempty"`
	Frame      string          `json:"frame,omitempty"`
	Type       string          `json:"type,omitempty"`
	ID         string          `json:"id,omitempty"`
	Payload    *StreamPayload  `json:"payload,omitempty"`
	Public     *PublicRequest  `json:"public,omitempty"`
	Upstream   *UpstreamTarget `json:"upstream,omitempty"`
	Route      *RouteMetadata  `json:"route,omitempty"`
	BodyLength int64           `json:"bodyLength,omitempty"`

	Kind      string      `json:"kind,omitempty"`
	RequestID string      `json:"requestId,omitempty"`
	Method    string      `json:"method,omitempty"`
	Path      string      `json:"path,omitempty"`
	Host      string      `json:"host,omitempty"`
	Target    string      `json:"target,omitempty"`
	Header    http.Header `json:"header,omitempty"`
}

type StreamResponse struct {
	V          int                 `json:"v,omitempty"`
	NS         string              `json:"ns,omitempty"`
	Frame      string              `json:"frame,omitempty"`
	Type       string              `json:"type,omitempty"`
	ID         string              `json:"id,omitempty"`
	Code       int                 `json:"code"`
	Msg        string              `json:"msg,omitempty"`
	Data       *StreamResponseData `json:"data,omitempty"`
	OK         bool                `json:"ok,omitempty"`
	StatusCode int                 `json:"statusCode,omitempty"`
	Headers    http.Header         `json:"headers,omitempty"`
	Header     http.Header         `json:"header,omitempty"`
	BodyLength int64               `json:"bodyLength,omitempty"`
	Error      string              `json:"error,omitempty"`
}

func NewStreamRequest(ns, frame, typ, id string, payload *StreamPayload) StreamRequest {
	return StreamRequest{
		V:       ProtocolVersion,
		NS:      ns,
		Frame:   frame,
		Type:    typ,
		ID:      id,
		Payload: payload,
	}
}

func NewStreamResponse(ns, frame, typ, id string, code int, msg string, data *StreamResponseData) StreamResponse {
	return StreamResponse{
		V:     ProtocolVersion,
		NS:    ns,
		Frame: frame,
		Type:  typ,
		ID:    id,
		Code:  code,
		Msg:   msg,
		Data:  data,
	}
}

func NewSuccessResponse(ns, typ, id string, data *StreamResponseData) StreamResponse {
	return NewStreamResponse(ns, FrameResponse, typ, id, 0, "success", data)
}

func NewErrorResponse(ns, typ, id string, code int, msg string) StreamResponse {
	if msg == "" {
		msg = "error"
	}
	return NewStreamResponse(ns, FrameError, typ, id, code, msg, nil)
}

func Int64Ptr(value int64) *int64 {
	return &value
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

func NewPublicRequest(req *http.Request, headers http.Header) PublicRequest {
	return PublicRequest{
		Method:  req.Method,
		Host:    NormalizeHost(req.Host),
		Path:    requestURI(req),
		Headers: headers,
	}
}

func StreamResponseHeaders(response StreamResponse) http.Header {
	if response.Data != nil && len(response.Data.Headers) > 0 {
		return response.Data.Headers
	}
	if len(response.Headers) > 0 {
		return response.Headers
	}
	return response.Header
}

func StreamResponseStatusCode(response StreamResponse) int {
	if response.Data != nil && response.Data.StatusCode > 0 {
		return response.Data.StatusCode
	}
	return response.StatusCode
}

func StreamResponseBodyLength(response StreamResponse) int64 {
	if response.Data != nil && response.Data.BodyLength != nil {
		return *response.Data.BodyLength
	}
	return response.BodyLength
}

func ParseUpstreamTarget(target string, websocket bool) (UpstreamTarget, error) {
	base, err := url.Parse(target)
	if err != nil {
		return UpstreamTarget{}, err
	}
	if base.Scheme == "" || base.Host == "" {
		return UpstreamTarget{}, fmt.Errorf("target must include scheme and host: %s", target)
	}
	scheme := base.Scheme
	if websocket {
		switch scheme {
		case "http":
			scheme = "ws"
		case "https":
			scheme = "wss"
		case "ws", "wss":
		default:
			return UpstreamTarget{}, fmt.Errorf("unsupported websocket target scheme: %s", base.Scheme)
		}
	} else if scheme != "http" && scheme != "https" {
		return UpstreamTarget{}, fmt.Errorf("unsupported http target scheme: %s", base.Scheme)
	}
	port := defaultPortForScheme(scheme)
	if rawPort := base.Port(); rawPort != "" {
		nextPort, err := strconv.Atoi(rawPort)
		if err != nil || nextPort <= 0 || nextPort > 65535 {
			return UpstreamTarget{}, fmt.Errorf("invalid target port: %s", rawPort)
		}
		port = nextPort
	}
	basePath := base.EscapedPath()
	if basePath == "/" {
		basePath = ""
	}
	return UpstreamTarget{
		Scheme:   scheme,
		Host:     base.Hostname(),
		Port:     port,
		BasePath: basePath,
	}, nil
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

func requestURI(req *http.Request) string {
	if req.URL == nil {
		return "/"
	}
	if uri := req.URL.RequestURI(); uri != "" {
		return uri
	}
	return "/"
}

func defaultPortForScheme(scheme string) int {
	switch scheme {
	case "http", "ws":
		return 80
	case "https", "wss":
		return 443
	default:
		return 0
	}
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
