package probe

import (
	"context"
	"log/slog"
	"net"
	"time"
)

// Server listens on a UDP port and responds to incoming probe requests.
type Server struct {
	listenAddr string
	conn       *net.UDPConn
	logger     *slog.Logger
}

// NewServer creates a probe server listening on the given address (e.g. ":9473").
func NewServer(listenAddr string, logger *slog.Logger) *Server {
	return &Server{
		listenAddr: listenAddr,
		logger:     logger,
	}
}

// ListenAndServe starts the UDP listener. It blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", s.listenAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	s.conn = conn

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, 256)
	for {
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Check if context was cancelled (normal shutdown).
			if ctx.Err() != nil {
				return nil
			}
			s.logger.Warn("probe server read error", "error", err)
			continue
		}
		recvTime := time.Now()

		pkt, err := Unmarshal(buf[:n])
		if err != nil {
			s.logger.Debug("probe server: bad packet", "from", remote, "error", err)
			continue
		}
		if pkt.Type != TypeRequest {
			continue
		}

		reply := NewReply(pkt, recvTime)
		data, err := reply.Marshal()
		if err != nil {
			s.logger.Warn("probe server: marshal error", "error", err)
			continue
		}
		if _, err := conn.WriteToUDP(data, remote); err != nil {
			s.logger.Warn("probe server: write error", "to", remote, "error", err)
		}
	}
}
