package yamux

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	frameOpen  byte = 1
	frameData  byte = 2
	frameClose byte = 3

	maxFrameSize = 1 << 20
)

type Config struct {
	EnableKeepAlive   bool
	KeepAliveInterval time.Duration
}

type Session struct {
	conn    net.Conn
	nextID  uint64
	writeMu sync.Mutex

	mu      sync.Mutex
	streams map[uint64]*Stream
	accept  chan *Stream
	closed  chan struct{}
	once    sync.Once
}

type Stream struct {
	id      uint64
	session *Session

	readMu sync.Mutex
	read   []byte
	readCh chan []byte

	closeOnce sync.Once
	closed    atomic.Bool
}

func DefaultConfig() *Config {
	return &Config{}
}

func Server(conn net.Conn, _ *Config) (*Session, error) {
	return newSession(conn, 2), nil
}

func Client(conn net.Conn, _ *Config) (*Session, error) {
	return newSession(conn, 1), nil
}

func newSession(conn net.Conn, nextID uint64) *Session {
	session := &Session{
		conn:    conn,
		nextID:  nextID,
		streams: make(map[uint64]*Stream),
		accept:  make(chan *Stream, 64),
		closed:  make(chan struct{}),
	}
	go session.readLoop()
	return session
}

func (s *Session) OpenStream() (*Stream, error) {
	if s.IsClosed() {
		return nil, io.ErrClosedPipe
	}
	s.mu.Lock()
	id := s.nextID
	s.nextID += 2
	stream := newStream(id, s)
	s.streams[id] = stream
	s.mu.Unlock()
	if err := s.writeFrame(frameOpen, id, nil); err != nil {
		s.removeStream(id)
		return nil, err
	}
	return stream, nil
}

func (s *Session) AcceptStream() (*Stream, error) {
	select {
	case stream := <-s.accept:
		return stream, nil
	case <-s.closed:
		return nil, io.ErrClosedPipe
	}
}

func (s *Session) Close() error {
	var err error
	s.once.Do(func() {
		close(s.closed)
		err = s.conn.Close()
		s.mu.Lock()
		for _, stream := range s.streams {
			stream.closeLocal()
		}
		s.streams = map[uint64]*Stream{}
		s.mu.Unlock()
	})
	return err
}

func (s *Session) CloseChan() <-chan struct{} {
	return s.closed
}

func (s *Session) IsClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func (s *Session) readLoop() {
	for {
		frameType, id, payload, err := s.readFrame()
		if err != nil {
			_ = s.Close()
			return
		}
		switch frameType {
		case frameOpen:
			stream := newStream(id, s)
			s.mu.Lock()
			s.streams[id] = stream
			s.mu.Unlock()
			select {
			case s.accept <- stream:
			case <-s.closed:
				return
			}
		case frameData:
			if stream := s.getStream(id); stream != nil {
				stream.push(payload)
			}
		case frameClose:
			if stream := s.getStream(id); stream != nil {
				stream.closeLocal()
			}
			s.removeStream(id)
		}
	}
}

func (s *Session) readFrame() (byte, uint64, []byte, error) {
	var header [13]byte
	if _, err := io.ReadFull(s.conn, header[:]); err != nil {
		return 0, 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[9:])
	if size > maxFrameSize {
		return 0, 0, nil, errors.New("yamux frame too large")
	}
	payload := make([]byte, size)
	if size > 0 {
		if _, err := io.ReadFull(s.conn, payload); err != nil {
			return 0, 0, nil, err
		}
	}
	return header[0], binary.BigEndian.Uint64(header[1:9]), payload, nil
}

func (s *Session) writeFrame(frameType byte, id uint64, payload []byte) error {
	if len(payload) > maxFrameSize {
		return errors.New("yamux frame too large")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var header [13]byte
	header[0] = frameType
	binary.BigEndian.PutUint64(header[1:9], id)
	binary.BigEndian.PutUint32(header[9:], uint32(len(payload)))
	if _, err := s.conn.Write(header[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := s.conn.Write(payload)
	return err
}

func (s *Session) getStream(id uint64) *Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

func (s *Session) removeStream(id uint64) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

func newStream(id uint64, session *Session) *Stream {
	return &Stream{
		id:      id,
		session: session,
		readCh:  make(chan []byte, 64),
	}
}

func (s *Stream) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	for len(s.read) == 0 {
		payload, ok := <-s.readCh
		if !ok {
			return 0, io.EOF
		}
		s.read = payload
	}
	n := copy(p, s.read)
	s.read = s.read[n:]
	return n, nil
}

func (s *Stream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxFrameSize {
			chunk = p[:maxFrameSize]
		}
		if err := s.session.writeFrame(frameData, s.id, chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

func (s *Stream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		err = s.session.writeFrame(frameClose, s.id, nil)
		s.session.removeStream(s.id)
		close(s.readCh)
	})
	return err
}

func (s *Stream) closeLocal() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.readCh)
	})
}

func (s *Stream) push(payload []byte) {
	select {
	case s.readCh <- payload:
	case <-s.session.closed:
	}
}

func (s *Stream) LocalAddr() net.Addr {
	return s.session.conn.LocalAddr()
}

func (s *Stream) RemoteAddr() net.Addr {
	return s.session.conn.RemoteAddr()
}

func (s *Stream) SetDeadline(time.Time) error {
	return nil
}

func (s *Stream) SetReadDeadline(time.Time) error {
	return nil
}

func (s *Stream) SetWriteDeadline(time.Time) error {
	return nil
}
