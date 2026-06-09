package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/linlay/zenmind-tunnel-server/internal/config"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
)

type Agent struct {
	Config config.AgentConfig
	Logger *slog.Logger
}

func RunAgent(ctx context.Context, cfg config.AgentConfig, logger *slog.Logger) error {
	agent := Agent{Config: cfg, Logger: logger}
	return agent.Run(ctx)
}

func (a Agent) Run(ctx context.Context) error {
	if a.Logger == nil {
		a.Logger = slog.Default()
	}
	if a.Config.Token == "" {
		return errors.New("AGENT_TOKEN is required")
	}
	reconnect := a.Config.ReconnectInterval
	if reconnect <= 0 {
		reconnect = 3 * time.Second
	}

	for {
		if err := a.connectOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			a.Logger.Error("agent connection failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnect):
		}
	}
}

func (a Agent) connectOnce(ctx context.Context) error {
	dialer := websocket.Dialer{}
	if a.Config.InsecureSkipVerify {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+a.Config.Token)
	ws, _, err := dialer.DialContext(ctx, a.Config.RelayURL, header)
	if err != nil {
		return err
	}
	defer ws.Close()

	conn := tunnel.NewWebSocketNetConn(ws)
	yamuxConfig := yamux.DefaultConfig()
	yamuxConfig.EnableKeepAlive = true
	yamuxConfig.KeepAliveInterval = 20 * time.Second
	session, err := yamux.Client(conn, yamuxConfig)
	if err != nil {
		return err
	}
	defer session.Close()
	a.Logger.Info("agent connected", "relay", a.Config.RelayURL)

	errs := make(chan error, 1)
	go func() {
		for {
			stream, err := session.AcceptStream()
			if err != nil {
				errs <- err
				return
			}
			go a.handleStream(stream)
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errs:
		return err
	case <-session.CloseChan():
		return errors.New("yamux session closed")
	}
}

func (a Agent) handleStream(stream *yamux.Stream) {
	defer stream.Close()
	var request tunnel.StreamRequest
	if err := tunnel.ReadJSON(stream, &request); err != nil {
		a.Logger.Error("read stream request", "error", err)
		return
	}
	switch request.Kind {
	case tunnel.KindHTTP:
		if err := a.handleHTTPStream(stream, request); err != nil {
			a.Logger.Error("handle http stream", "error", err)
		}
	case tunnel.KindWebSocket:
		if err := a.handleWebSocketStream(stream, request); err != nil {
			a.Logger.Error("handle websocket stream", "error", err)
		}
	default:
		_ = tunnel.WriteJSON(stream, tunnel.StreamResponse{
			OK:         false,
			StatusCode: http.StatusBadRequest,
			Error:      "unknown stream kind",
		})
	}
}

func (a Agent) handleHTTPStream(stream io.ReadWriter, request tunnel.StreamRequest) error {
	targetURL, err := tunnel.BuildTargetURL(request.Target, request.Path, false)
	if err != nil {
		return tunnel.WriteJSON(stream, tunnel.StreamResponse{OK: false, StatusCode: http.StatusBadGateway, Error: err.Error()})
	}
	var body io.Reader = http.NoBody
	if request.BodyLength > 0 {
		body = io.LimitReader(stream, request.BodyLength)
	}
	outReq, err := http.NewRequest(request.Method, targetURL, body)
	if err != nil {
		return tunnel.WriteJSON(stream, tunnel.StreamResponse{OK: false, StatusCode: http.StatusBadGateway, Error: err.Error()})
	}
	outReq.Header = tunnel.StripHopHeaders(request.Header)
	outReq.Host = targetHost(targetURL)
	outReq.Header.Set("X-Forwarded-Host", request.Host)
	outReq.Header.Set("X-Zenm-Request-ID", request.RequestID)
	if request.BodyLength >= 0 {
		outReq.ContentLength = request.BodyLength
	}

	resp, err := http.DefaultClient.Do(outReq)
	if err != nil {
		return tunnel.WriteJSON(stream, tunnel.StreamResponse{OK: false, StatusCode: http.StatusBadGateway, Error: err.Error()})
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return tunnel.WriteJSON(stream, tunnel.StreamResponse{OK: false, StatusCode: http.StatusBadGateway, Error: err.Error()})
	}
	if err := tunnel.WriteJSON(stream, tunnel.StreamResponse{
		OK:         true,
		StatusCode: resp.StatusCode,
		Header:     tunnel.StripHopHeaders(resp.Header),
		BodyLength: int64(len(responseBody)),
	}); err != nil {
		return err
	}
	if len(responseBody) == 0 {
		return nil
	}
	_, err = stream.Write(responseBody)
	return err
}

func (a Agent) handleWebSocketStream(stream io.ReadWriter, request tunnel.StreamRequest) error {
	targetURL, err := tunnel.BuildTargetURL(request.Target, request.Path, true)
	if err != nil {
		return tunnel.WriteJSON(stream, tunnel.StreamResponse{OK: false, StatusCode: http.StatusBadGateway, Error: err.Error()})
	}
	header := tunnel.StripWebSocketDialHeaders(request.Header)
	header.Set("X-Forwarded-Host", request.Host)
	header.Set("X-Zenm-Request-ID", request.RequestID)
	localWS, resp, err := websocket.DefaultDialer.Dial(targetURL, header)
	if err != nil {
		status := http.StatusBadGateway
		if resp != nil {
			status = resp.StatusCode
		}
		return tunnel.WriteJSON(stream, tunnel.StreamResponse{OK: false, StatusCode: status, Error: err.Error()})
	}
	defer localWS.Close()
	if err := tunnel.WriteJSON(stream, tunnel.StreamResponse{OK: true, StatusCode: http.StatusSwitchingProtocols}); err != nil {
		return err
	}

	errs := make(chan error, 2)
	go func() { errs <- tunnel.CopyWebSocketToFrames(localWS, stream) }()
	go func() { errs <- tunnel.CopyFramesToWebSocket(stream, localWS) }()
	err = <-errs
	a.Logger.Debug("local websocket stream closed", "error", err)
	return nil
}

func targetHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.User = nil
	return fmt.Sprintf("%s://%s%s", parsed.Scheme, parsed.Host, parsed.Path)
}
