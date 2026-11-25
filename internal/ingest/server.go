package ingest

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"sync/atomic"
)

// Server accepts RemoteFmt lines over TCP (newline-delimited) and UDP (one line per datagram).
// It never blocks the sender for long: when the consumer falls behind, lines are dropped and counted,
// so a slow noc-bot cannot back-pressure rsyslog (which forwards through its own discarding queue too).
type Server struct {
	Out     chan Line
	Dropped atomic.Uint64
	Bad     atomic.Uint64
	Log     *slog.Logger
}

// NewServer makes a server whose output channel holds up to buf lines.
func NewServer(buf int, log *slog.Logger) *Server {
	return &Server{Out: make(chan Line, buf), Log: log}
}

func (s *Server) push(raw string) {
	l, err := Parse(raw)
	if err != nil {
		s.Bad.Add(1)
		return
	}
	select {
	case s.Out <- l:
	default:
		s.Dropped.Add(1)
	}
}

// ServeTCP accepts connections until ctx is done.
func (s *Server) ServeTCP(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.Log.Warn("accept", "err", err)
			continue
		}
		go func() {
			defer c.Close()
			sc := bufio.NewScanner(c)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				s.push(sc.Text())
			}
		}()
	}
}

// ServeUDP reads datagrams until ctx is done.
func (s *Server) ServeUDP(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); pc.Close() }()
	buf := make([]byte, 65536)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		s.push(string(buf[:n]))
	}
}
