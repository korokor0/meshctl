package probe

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// PingResult holds the outcome of an ICMP echo probe.
type PingResult struct {
	Peer string
	RTT  time.Duration
	OK   bool
}

// Ping sends an ICMP echo request to addr and measures RTT.
// This is the fallback for non-agent peers (RouterOS, static).
// Requires CAP_NET_RAW or root.
func Ping(ctx context.Context, peer, addr string, timeout time.Duration) PingResult {
	conn, err := icmp.ListenPacket("udp4", "")
	if err != nil {
		return PingResult{Peer: peer, OK: false}
	}
	defer conn.Close()

	dst, err := net.ResolveIPAddr("ip4", addr)
	if err != nil {
		return PingResult{Peer: peer, OK: false}
	}

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   os.Getpid() & 0xffff,
			Seq:  1,
			Data: []byte("meshctl-probe"),
		},
	}
	data, err := msg.Marshal(nil)
	if err != nil {
		return PingResult{Peer: peer, OK: false}
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)

	start := time.Now()
	if _, err := conn.WriteTo(data, &net.UDPAddr{IP: dst.IP}); err != nil {
		return PingResult{Peer: peer, OK: false}
	}

	buf := make([]byte, 1500)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		return PingResult{Peer: peer, OK: false}
	}
	rtt := time.Since(start)

	reply, err := icmp.ParseMessage(1, buf[:n])
	if err != nil {
		return PingResult{Peer: peer, OK: false}
	}
	if reply.Type != ipv4.ICMPTypeEchoReply {
		return PingResult{Peer: peer, OK: false}
	}

	_ = fmt.Sprintf("ping %s: %v", peer, rtt) // suppress unused import
	return PingResult{Peer: peer, RTT: rtt, OK: true}
}
