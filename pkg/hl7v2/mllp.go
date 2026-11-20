package hl7v2

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// MLLP v1 framing control bytes (HL7 Minimal Lower Layer Protocol).
const (
	MLLPStartBlock     byte = 0x0B // <VT> vertical tab
	MLLPEndBlock       byte = 0x1C // <FS> file separator
	MLLPCarriageReturn byte = 0x0D // <CR> carriage return

	DefaultMaxFrameSize = MaxMessageBytes
)

var (
	// ErrInvalidMLLPFrame indicates missing <VT> start byte or missing <CR> after <FS>.
	ErrInvalidMLLPFrame = errors.New("hl7v2: invalid MLLP frame")
	// ErrMLLPFrameTooLarge indicates an MLLP payload exceeding the configured maximum frame size.
	ErrMLLPFrameTooLarge = errors.New("hl7v2: MLLP frame exceeds maximum size limit")
)

// ReadMLLPFrame reads a single MLLP v1 frame (<VT>payload<FS><CR>) from br.
// Returns io.EOF cleanly when the peer closes the connection between frames.
func ReadMLLPFrame(br *bufio.Reader, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFrameSize
	}
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == MLLPStartBlock {
			break
		}
		// Tolerate inter-frame CR/LF whitespace from legacy interface engines; reject other stray bytes.
		if b == '\r' || b == '\n' {
			continue
		}
		return nil, fmt.Errorf("%w: expected start block 0x0B, got 0x%02X", ErrInvalidMLLPFrame, b)
	}

	buf := make([]byte, 0, 1024)
	for {
		b, err := br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%w: unexpected EOF before end block 0x1C", ErrInvalidMLLPFrame)
			}
			return nil, err
		}
		if b == MLLPEndBlock {
			cr, err := br.ReadByte()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil, fmt.Errorf("%w: missing trailing 0x0D after 0x1C", ErrInvalidMLLPFrame)
				}
				return nil, err
			}
			if cr != MLLPCarriageReturn {
				return nil, fmt.Errorf("%w: expected 0x0D after 0x1C, got 0x%02X", ErrInvalidMLLPFrame, cr)
			}
			return buf, nil
		}
		if len(buf) >= maxBytes {
			return nil, ErrMLLPFrameTooLarge
		}
		buf = append(buf, b)
	}
}

// WriteMLLPFrame writes a single MLLP v1 frame (<VT>payload<FS><CR>) to w.
func WriteMLLPFrame(w io.Writer, payload []byte) error {
	frame := make([]byte, 0, len(payload)+3)
	frame = append(frame, MLLPStartBlock)
	frame = append(frame, payload...)
	frame = append(frame, MLLPEndBlock, MLLPCarriageReturn)
	_, err := w.Write(frame)
	return err
}

// MLLPHandlerFunc processes a single unframed HL7 v2 ER7 message and returns the ER7 ACK bytes.
type MLLPHandlerFunc func(ctx context.Context, rawMessage []byte) ([]byte, error)

// MLLPConfig configures an MLLPServer TCP listener.
type MLLPConfig struct {
	Addr         string
	MaxFrameSize int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	Logger       *slog.Logger
	Handler      MLLPHandlerFunc
}

// MLLPServer is a concurrent, bounded, gracefully drainable HL7 v2 MLLP v1 TCP server.
type MLLPServer struct {
	cfg      MLLPConfig
	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	baseCtx  context.Context
	cancel   context.CancelFunc
	closed   atomic.Bool
	bindAddr string
}

// NewMLLPServer creates a new MLLP v1 TCP server.
func NewMLLPServer(cfg MLLPConfig) *MLLPServer {
	if cfg.MaxFrameSize <= 0 {
		cfg.MaxFrameSize = DefaultMaxFrameSize
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &MLLPServer{
		cfg:     cfg,
		conns:   make(map[net.Conn]struct{}),
		baseCtx: ctx,
		cancel:  cancel,
	}
}

// Start binds the TCP listener and begins accepting MLLP connections in the background.
func (s *MLLPServer) Start() error {
	addr := s.cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.bindAddr = ln.Addr().String()
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = s.acceptLoop(ln)
	}()
	return nil
}

// ListenAndServe binds cfg.Addr and blocks serving MLLP frames until Shutdown/Close is called.
func (s *MLLPServer) ListenAndServe() error {
	addr := s.cfg.Addr
	if addr == "" {
		addr = ":2575"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve accepts MLLP connections on ln until closed.
func (s *MLLPServer) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.bindAddr = ln.Addr().String()
	s.mu.Unlock()
	return s.acceptLoop(ln)
}

// Addr returns the bound TCP address (useful when started with ":0").
func (s *MLLPServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindAddr
}

func (s *MLLPServer) acceptLoop(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed.Load() {
			s.mu.Unlock()
			_ = conn.Close()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
			}()
			s.handleConn(c)
		}(conn)
	}
}

func (s *MLLPServer) handleConn(conn net.Conn) {
	br := bufio.NewReaderSize(conn, 64*1024)
	for {
		if s.closed.Load() {
			return
		}
		if s.cfg.ReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		}
		payload, err := ReadMLLPFrame(br, s.cfg.MaxFrameSize)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || s.closed.Load() {
				return
			}
			// Send an AR NACK if possible before closing a framing-corrupted stream
			nack := BuildACK(nil, "", ACKReject, err.Error(), "207", time.Now().UTC())
			if s.cfg.WriteTimeout > 0 {
				_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
			}
			_ = WriteMLLPFrame(conn, []byte(nack))
			return
		}

		var ack []byte
		if s.cfg.Handler != nil {
			var hErr error
			ack, hErr = s.cfg.Handler(s.baseCtx, payload)
			if hErr != nil && len(ack) == 0 {
				msg, _ := ParseBytes(payload)
				ack = []byte(BuildACK(msg, string(payload), ACKError, hErr.Error(), "207", time.Now().UTC()))
			}
		} else {
			msg, pErr := ParseBytes(payload)
			if pErr != nil {
				ack = []byte(BuildACK(nil, string(payload), ACKReject, pErr.Error(), "100", time.Now().UTC()))
			} else {
				ack = []byte(BuildACK(msg, string(payload), ACKAccept, "Message accepted", "", time.Now().UTC()))
			}
		}

		if s.cfg.WriteTimeout > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		}
		if wErr := WriteMLLPFrame(conn, ack); wErr != nil {
			if s.cfg.Logger != nil && !s.closed.Load() {
				s.cfg.Logger.Warn("failed to write MLLP ACK frame", "error", wErr)
			}
			return
		}
	}
}

// Shutdown gracefully stops accepting new connections, unblocks active connections, and waits for drain.
func (s *MLLPServer) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if s.closed.CompareAndSwap(false, true) {
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Lock()
		if s.ln != nil {
			_ = s.ln.Close()
		}
		for c := range s.conns {
			_ = c.SetDeadline(time.Now())
			_ = c.Close()
		}
		s.mu.Unlock()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close shuts down the MLLP server with a 5-second drain timeout.
func (s *MLLPServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}
