package probe

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client sends UDP probes to peers and collects results.
type Client struct {
	probePort int
	timeout   time.Duration
	logger    *slog.Logger
	seq       atomic.Uint32
}

// NewClient creates a probe client.
func NewClient(probePort int, timeout time.Duration, logger *slog.Logger) *Client {
	return &Client{
		probePort: probePort,
		timeout:   timeout,
		logger:    logger,
	}
}

// ProbeAll sends a probe to each peer address concurrently and returns results.
// peerAddrs maps peer name to its WireGuard tunnel address (IP only, no port).
func (c *Client) ProbeAll(ctx context.Context, peerAddrs map[string]string) []ProbeResult {
	var (
		mu      sync.Mutex
		results []ProbeResult
		wg      sync.WaitGroup
	)

	for name, addr := range peerAddrs {
		wg.Add(1)
		go func(name, addr string) {
			defer wg.Done()
			result, err := c.ProbePeer(ctx, name, addr)
			if err != nil {
				c.logger.Debug("probe failed", "peer", name, "error", err)
				mu.Lock()
				results = append(results, ProbeResult{Peer: name, Valid: false})
				mu.Unlock()
				return
			}
			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(name, addr)
	}
	wg.Wait()
	return results
}

// ProbePeer sends a single UDP probe to a peer and waits for the reply.
// addr may be an IPv4 address, IPv6 address, or IPv6 with zone ID
// (e.g. "fe80::127:3%igp-hkg").
func (c *Client) ProbePeer(ctx context.Context, peer, addr string) (ProbeResult, error) {
	// IPv6 addresses (containing ":") need brackets for host:port parsing.
	var target string
	if strings.ContainsRune(addr, ':') {
		target = fmt.Sprintf("[%s]:%d", addr, c.probePort)
	} else {
		target = fmt.Sprintf("%s:%d", addr, c.probePort)
	}
	raddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("resolve %s: %w", target, err)
	}

	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("dial %s: %w", target, err)
	}
	defer conn.Close()

	seq := uint16(c.seq.Add(1))
	req := NewRequest(seq)

	data, err := req.Marshal()
	if err != nil {
		return ProbeResult{}, fmt.Errorf("marshal: %w", err)
	}

	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)

	if _, err := conn.Write(data); err != nil {
		return ProbeResult{}, fmt.Errorf("write: %w", err)
	}

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("read: %w", err)
	}
	t4 := time.Now().UnixNano()

	reply, err := Unmarshal(buf[:n])
	if err != nil {
		return ProbeResult{}, fmt.Errorf("unmarshal reply: %w", err)
	}
	if reply.Type != TypeReply || reply.Seq != seq {
		return ProbeResult{}, fmt.Errorf("unexpected reply: type=%d seq=%d (expected %d)", reply.Type, reply.Seq, seq)
	}

	return ComputeResult(peer, reply.T1, reply.T2, reply.T3, t4), nil
}
