package bus

import (
	"strings"
	"testing"
)

func setupFaultBus(t *testing.T, fault SlaveFault, timeout int) (*Bus, *Runner, *Master, *Slave, *ManualDevice) {
	t.Helper()
	b := New()
	m := NewMaster("m", b, 0)
	m.SetClockStretchTimeout(timeout)
	m.QueueWrite(Frame{Addr: 0x10, Data: []byte{0xA5}})
	s := NewSlave("s10", 0x10)
	s.SetFault(fault)
	w := NewManualDevice("w", nil)
	r := NewRunner(b, m, s, NewArbiter("arb", m), w)
	return b, r, m, s, w
}

func wave(entries []Entry) string {
	out := strings.Builder{}
	for _, entry := range entries {
		if entry.Kind == "level" {
			out.WriteString(entry.SCL.String())
			out.WriteString(entry.SDA.String())
			out.WriteByte(' ')
		}
	}
	return out.String()
}

func hasEvent(b *Bus, kind string) bool {
	for _, entry := range b.Trace {
		if entry.Kind == kind {
			return true
		}
	}
	return false
}

func TestSlaveClockStretchDelaysButPreservesTransfer(t *testing.T) {
	normalBus, normalRunner, normalMaster, normalSlave, _ := setupFaultBus(t, SlaveFault{}, 0)
	waitDone(t, normalMaster.Done(), 100, normalRunner)

	faultBus, faultRunner, faultMaster, faultSlave, witness := setupFaultBus(t, SlaveFault{StretchAt: 17, StretchTicks: 6}, 0)
	result := waitDone(t, faultMaster.Done(), 120, faultRunner)
	if result.Err != nil {
		t.Fatalf("stretched transfer: %v", result.Err)
	}
	if got := faultSlave.Bytes(); len(got) != 1 || got[0] != 0xA5 {
		t.Fatalf("slave got %v, want A5", got)
	}
	if got := normalSlave.Bytes(); len(got) != 1 || got[0] != 0xA5 {
		t.Fatalf("normal slave got %v, want A5", got)
	}
	if !hasEvent(faultBus, EvClockWait) {
		t.Fatal("missing clock-wait event")
	}
	highWhileWait := false
	for _, snap := range witness.Snapshots() {
		if snap.Tick >= 17 && snap.Tick < 23 && snap.SCL == High {
			highWhileWait = true
		}
	}
	if highWhileWait {
		t.Fatal("master treated its released SCL as completed high while slave stretched")
	}
	if faultBus.Now <= normalBus.Now {
		t.Fatalf("fault finish tick %d, want later than normal %d", faultBus.Now, normalBus.Now)
	}
	if got := faultSlave.Bytes()[0]; got != normalSlave.Bytes()[0] {
		t.Fatalf("fault byte %#x != normal %#x", got, normalSlave.Bytes()[0])
	}
}

func TestSlaveDataHoldRecoveryAndTimeoutDoNotStartNewTransfer(t *testing.T) {
	fault := SlaveFault{StretchAt: 22, StretchTicks: 6, HoldAt: 20, HoldTicks: 8, ClearAt: 28}
	b, r, m, s, _ := setupFaultBus(t, fault, 6)
	result := waitDone(t, m.Done(), 130, r)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "timeout") {
		t.Fatalf("result %v, want timeout", result.Err)
	}
	if m.Active() {
		t.Fatal("master started or continued a transfer after timeout")
	}
	if !hasEvent(b, EvClockWait) || !hasEvent(b, EvClockTimeout) {
		t.Fatalf("events=%v, want wait and timeout", eventKinds(b))
	}
	s.ClearFault()
	r.Run(4)
	if m.Active() || len(s.Bytes()) != 0 {
		t.Fatal("clearing fault unexpectedly resumed an aborted transfer")
	}
}

func TestMultipleRecoveriesOnlyOneCanOwnBus(t *testing.T) {
	b := New()
	holder := &holdingDevice{id: "holder", releaseAt: 60}
	r1 := NewRecovery("r1", 17)
	r2 := NewRecovery("r2", 17)
	group := NewRecoveryGroup("group", r1, r2)
	r := NewRunner(b, holder, r1, r2, group)
	r.Run(65)

	success := 0
	failed := 0
	lost := 0
	for _, entry := range b.Trace {
		switch entry.Kind {
		case EvRecoverDone:
			success++
		case EvRecoverFailed:
			failed++
		case EvRecoverLost:
			lost++
		}
	}
	if success != 1 || failed != 0 || lost != 1 {
		t.Fatalf("recovery done=%d failed=%d lost=%d, want one success and one loss", success, failed, lost)
	}
	if r1.Done() == r2.Done() || r1.Failed() == r2.Failed() {
		t.Fatal("simultaneous recoveries both reported the same outcome")
	}
}

func TestCheckpointMidBitMatchesContinuousWaveAndArbitration(t *testing.T) {
	build := func() (*Bus, *Runner, *Master, *Master, *Slave, *Slave) {
		b := New()
		loser := NewMaster("slow", b, 0)
		loser.QueueWrite(Frame{Addr: 0x11, Data: []byte{0x11}})
		winner := NewMaster("fast", b, 0)
		winner.QueueWrite(Frame{Addr: 0x10, Data: []byte{0x22}})
		s11 := NewSlave("s11", 0x11)
		s10 := NewSlave("s10", 0x10)
		r := NewRunner(b, loser, winner, NewArbiter("arb", loser, winner), s10, s11)
		_ = r
		return b, r, loser, winner, s11, s10
	}

	continuousBus, continuousRunner, continuousLoser, continuousWinner, _, _ := build()
	continuousRunner.Run(100)

	b, r, loser, winner, _, _ := build()
	r.Run(22)
	cp := b.Checkpoint()
	r.Run(1)
	b.Restore(cp)
	r.Run(78)

	if wave(b.Trace) != wave(continuousBus.Trace) {
		t.Fatalf("checkpoint wave:\n%s\ncontinuous:\n%s", wave(b.Trace), wave(continuousBus.Trace))
	}
	if loser.LostAt() != continuousLoser.LostAt() || winner.Result().Err != continuousWinner.Result().Err {
		t.Fatalf("checkpoint arbitration loser=%d winnerErr=%v, continuous loser=%d winnerErr=%v",
			loser.LostAt(), winner.Result().Err, continuousLoser.LostAt(), continuousWinner.Result().Err)
	}
	if !loser.Lost() || continuousWinner.Result().Err != nil {
		t.Fatal("checkpoint changed arbitration outcome")
	}
}
