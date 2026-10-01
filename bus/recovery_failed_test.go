package bus

import (
	"strings"
	"testing"
)

func TestFailedRecoveryBlocksNewTransfer(t *testing.T) {
	b := New()
	m := NewMaster("m", b, 0)
	m.QueueWrite(Frame{Addr: 0x10, Data: []byte{1}})
	holder := &permanentHolder{id: "holder"}
	recovery := NewRecovery("recover", 17)
	arbiter := NewArbiter("arb", m)
	arbiter.SuppressRecovery(recovery)
	r := NewRunner(b, m, holder, recovery, arbiter)
	r.Run(65)

	if !recovery.Failed() || recovery.Done() {
		t.Fatal("permanent SDA hold should fail recovery, not report STOP success")
	}
	if !m.Blocked() || m.Active() {
		t.Fatalf("master blocked=%v active=%v after failed recovery", m.Blocked(), m.Active())
	}
	if result := m.Result(); result.Err == nil || !strings.Contains(result.Err.Error(), "failed bus recovery") {
		t.Fatalf("result=%v, want failed recovery error", result.Err)
	}
}
