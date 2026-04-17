// Package probe implements the meshctl UDP probe protocol for one-way delay
// measurement and ICMP echo fallback for non-agent peers.
package probe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const (
	// Magic is the protocol identifier: "MCTP" (meshctl probe).
	Magic uint32 = 0x4D435450

	// Version is the current protocol version.
	Version uint8 = 1

	// TypeRequest identifies a probe request packet.
	TypeRequest uint8 = 0x01

	// TypeReply identifies a probe reply packet.
	TypeReply uint8 = 0x02

	// RequestSize is the wire size of a request packet (32 bytes).
	RequestSize = 32

	// ReplySize is the wire size of a reply packet (48 bytes).
	ReplySize = 48
)

// Packet represents a probe packet on the wire.
// All timestamps are nanoseconds since Unix epoch.
type Packet struct {
	Magic   uint32
	Version uint8
	Type    uint8  // TypeRequest or TypeReply
	Seq     uint16 // sequence number (wraps)
	T1      int64  // sender's transmit timestamp (always present)
	T2      int64  // responder's receive timestamp (reply only, 0 in request)
	T3      int64  // responder's transmit timestamp (reply only, 0 in request)
}

// NewRequest creates a probe request packet with the given sequence number
// and the current time as T1.
func NewRequest(seq uint16) Packet {
	return Packet{
		Magic:   Magic,
		Version: Version,
		Type:    TypeRequest,
		Seq:     seq,
		T1:      time.Now().UnixNano(),
	}
}

// NewReply creates a reply from a received request, filling T2 (receive time)
// and T3 (transmit time).
func NewReply(req Packet, recvTime time.Time) Packet {
	return Packet{
		Magic:   Magic,
		Version: Version,
		Type:    TypeReply,
		Seq:     req.Seq,
		T1:      req.T1,
		T2:      recvTime.UnixNano(),
		T3:      time.Now().UnixNano(),
	}
}

// Marshal serializes a packet to wire format.
// Request packets produce 32 bytes; reply packets produce 48 bytes.
func (p *Packet) Marshal() ([]byte, error) {
	switch p.Type {
	case TypeRequest:
		buf := make([]byte, RequestSize)
		binary.BigEndian.PutUint32(buf[0:4], p.Magic)
		buf[4] = p.Version
		buf[5] = p.Type
		binary.BigEndian.PutUint16(buf[6:8], p.Seq)
		binary.BigEndian.PutUint64(buf[8:16], uint64(p.T1))
		// bytes 16-31 are padding (zero)
		return buf, nil
	case TypeReply:
		buf := make([]byte, ReplySize)
		binary.BigEndian.PutUint32(buf[0:4], p.Magic)
		buf[4] = p.Version
		buf[5] = p.Type
		binary.BigEndian.PutUint16(buf[6:8], p.Seq)
		binary.BigEndian.PutUint64(buf[8:16], uint64(p.T1))
		binary.BigEndian.PutUint64(buf[16:24], uint64(p.T2))
		binary.BigEndian.PutUint64(buf[24:32], uint64(p.T3))
		// bytes 32-47 are padding (zero)
		return buf, nil
	default:
		return nil, fmt.Errorf("unknown packet type: %d", p.Type)
	}
}

// Unmarshal parses a wire-format packet. The input must be at least
// RequestSize (32) bytes. Reply packets must be at least ReplySize (48) bytes.
func Unmarshal(data []byte) (Packet, error) {
	if len(data) < RequestSize {
		return Packet{}, errors.New("packet too short")
	}

	var p Packet
	p.Magic = binary.BigEndian.Uint32(data[0:4])
	if p.Magic != Magic {
		return Packet{}, fmt.Errorf("bad magic: 0x%08X", p.Magic)
	}
	p.Version = data[4]
	if p.Version != Version {
		return Packet{}, fmt.Errorf("unsupported version: %d", p.Version)
	}
	p.Type = data[5]
	p.Seq = binary.BigEndian.Uint16(data[6:8])
	p.T1 = int64(binary.BigEndian.Uint64(data[8:16]))

	if p.Type == TypeReply {
		if len(data) < ReplySize {
			return Packet{}, errors.New("reply packet too short")
		}
		p.T2 = int64(binary.BigEndian.Uint64(data[16:24]))
		p.T3 = int64(binary.BigEndian.Uint64(data[24:32]))
	}

	return p, nil
}

// ProbeResult holds the computed delays from a completed probe exchange.
type ProbeResult struct {
	Peer         string
	ForwardDelay time.Duration // T2 - T1 (one-way, A→B)
	ReturnDelay  time.Duration // T4 - T3 (one-way, B→A)
	RTT          time.Duration // (T4-T1) - (T3-T2), processing excluded
	ClockOffset  time.Duration // ((T2-T1) - (T4-T3)) / 2
	Valid        bool          // false if forward delay is negative (clock skew)
}

// ComputeResult calculates delays from the four timestamps of a probe exchange.
func ComputeResult(peer string, t1, t2, t3, t4 int64) ProbeResult {
	forward := time.Duration(t2 - t1)
	ret := time.Duration(t4 - t3)
	rtt := time.Duration((t4 - t1) - (t3 - t2))
	offset := time.Duration(((t2 - t1) - (t4 - t3)) / 2)

	return ProbeResult{
		Peer:         peer,
		ForwardDelay: forward,
		ReturnDelay:  ret,
		RTT:          rtt,
		ClockOffset:  offset,
		Valid:        forward > 0,
	}
}
