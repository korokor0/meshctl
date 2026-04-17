package probe

import (
	"testing"
	"time"
)

func TestPacketMarshalUnmarshal_Request(t *testing.T) {
	req := Packet{
		Magic:   Magic,
		Version: Version,
		Type:    TypeRequest,
		Seq:     42,
		T1:      time.Now().UnixNano(),
	}
	data, err := req.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(data) != RequestSize {
		t.Fatalf("expected %d bytes, got %d", RequestSize, len(data))
	}

	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Magic != Magic {
		t.Errorf("magic: got 0x%08X", got.Magic)
	}
	if got.Type != TypeRequest {
		t.Errorf("type: got %d", got.Type)
	}
	if got.Seq != 42 {
		t.Errorf("seq: got %d", got.Seq)
	}
	if got.T1 != req.T1 {
		t.Errorf("T1 mismatch")
	}
}

func TestPacketMarshalUnmarshal_Reply(t *testing.T) {
	reply := Packet{
		Magic:   Magic,
		Version: Version,
		Type:    TypeReply,
		Seq:     100,
		T1:      1000,
		T2:      2000,
		T3:      3000,
	}
	data, err := reply.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(data) != ReplySize {
		t.Fatalf("expected %d bytes, got %d", ReplySize, len(data))
	}

	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.T1 != 1000 || got.T2 != 2000 || got.T3 != 3000 {
		t.Errorf("timestamp mismatch: T1=%d T2=%d T3=%d", got.T1, got.T2, got.T3)
	}
}

func TestUnmarshal_TooShort(t *testing.T) {
	_, err := Unmarshal([]byte{0x01, 0x02})
	if err == nil {
		t.Error("expected error for short packet")
	}
}

func TestUnmarshal_BadMagic(t *testing.T) {
	data := make([]byte, RequestSize)
	// Wrong magic.
	data[0] = 0xFF
	_, err := Unmarshal(data)
	if err == nil {
		t.Error("expected error for bad magic")
	}
}

func TestNewReply(t *testing.T) {
	req := NewRequest(7)
	recvTime := time.Now()
	reply := NewReply(req, recvTime)

	if reply.Type != TypeReply {
		t.Errorf("expected reply type, got %d", reply.Type)
	}
	if reply.Seq != 7 {
		t.Errorf("seq mismatch: got %d", reply.Seq)
	}
	if reply.T1 != req.T1 {
		t.Error("T1 should be copied from request")
	}
	if reply.T2 != recvTime.UnixNano() {
		t.Error("T2 should be receive time")
	}
	if reply.T3 == 0 {
		t.Error("T3 should be set")
	}
}

func TestComputeResult(t *testing.T) {
	// Simulate: A sends at t=100, B receives at t=110, B sends at t=115, A receives at t=125.
	// All in nanoseconds for simplicity.
	t1 := int64(100)
	t2 := int64(110)
	t3 := int64(115)
	t4 := int64(125)

	r := ComputeResult("peer1", t1, t2, t3, t4)

	if r.ForwardDelay != 10 {
		t.Errorf("forward delay: expected 10, got %d", r.ForwardDelay)
	}
	if r.ReturnDelay != 10 {
		t.Errorf("return delay: expected 10, got %d", r.ReturnDelay)
	}
	// RTT = (T4-T1) - (T3-T2) = 25 - 5 = 20
	if r.RTT != 20 {
		t.Errorf("RTT: expected 20, got %d", r.RTT)
	}
	// Offset = ((T2-T1) - (T4-T3)) / 2 = (10 - 10) / 2 = 0
	if r.ClockOffset != 0 {
		t.Errorf("clock offset: expected 0, got %d", r.ClockOffset)
	}
	if !r.Valid {
		t.Error("expected valid result")
	}
}

func TestComputeResult_NegativeForward(t *testing.T) {
	// Clock skew: B's clock is behind A's.
	r := ComputeResult("peer1", 200, 100, 105, 210)
	if r.Valid {
		t.Error("expected invalid result for negative forward delay")
	}
}
