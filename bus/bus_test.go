package bus

import (
	"strings"
	"testing"
)

func setup(t *testing.T, devs ...Device) (*Bus, *Runner) {
	t.Helper()
	b := New()
	r := NewRunner(b, devs...)
	return b, r
}

func waitDone(t *testing.T, ch <-chan TxResult, ticks int, r *Runner) TxResult {
	t.Helper()
	for i := 0; i < ticks; i++ {
		select {
		case result := <-ch:
			return result
		default:
		}
		r.Step()
	}
	t.Fatal("transaction did not finish")
	return TxResult{}
}

func eventKinds(b *Bus) []string {
	out := []string{}
	for _, entry := range b.Trace {
		if entry.Kind != "level" {
			out = append(out, entry.Kind+":"+entry.Who)
		}
	}
	return out
}

func TestWriteAckAndManualPhaseOwnership(t *testing.T) {
	b := New()
	master := NewMaster("m0", b, 0)
	master.QueueWrite(Frame{Addr: 0x10, Data: []byte{0xA5}})
	slave := NewSlave("s10", 0x10)
	witness := NewManualDevice("witness", nil)
	r := NewRunner(b, master, slave, NewArbiter("arb", master), witness)

	result := waitDone(t, master.Done(), 100, r)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if got := slave.Bytes(); len(got) != 1 || got[0] != 0xA5 {
		t.Fatalf("slave got %v, want [A5]", got)
	}
	if !b.Idle() {
		t.Fatalf("bus = (%s,%s), want idle", b.scl, b.sda)
	}

	startSeen := false
	ackOwners := []string{}
	for _, snap := range witness.Snapshots() {
		if snap.StartCondition() {
			startSeen = true
			if len(snap.OwnersSDA) != 1 || snap.OwnersSDA[0] != "m0" {
				t.Fatalf("START owners = %v, want m0", snap.OwnersSDA)
			}
		}
		if snap.Tick == 78 && snap.Phase == 1 {
			ackOwners = append(ackOwners, snap.OwnersSDA...)
			if snap.SCL != High {
				t.Fatalf("ACK SCL = %s, want high master clock pulse", snap.SCL)
			}
		}
	}
	if !startSeen {
		t.Fatal("witness did not observe START")
	}
	if len(ackOwners) != 1 || ackOwners[0] != "s10" {
		t.Fatalf("ACK SDA owners = %v, want s10", ackOwners)
	}
}

func TestNACKFinishesWithStop(t *testing.T) {
	b := New()
	master := NewMaster("lonely", b, 0)
	master.QueueWrite(Frame{Addr: 0x77, Data: []byte{1}})
	r := NewRunner(b, master, NewArbiter("arb", master))

	result := waitDone(t, master.Done(), 80, r)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "NACK") {
		t.Fatalf("result = %v, want NACK", result.Err)
	}
	if !b.Idle() {
		t.Fatalf("bus after NACK = (%s,%s), want idle", b.scl, b.sda)
	}
}

func TestArbitrationLoserWaitsRetriesAndOnlyWinnerOwnsBus(t *testing.T) {
	b := New()
	loser := NewMaster("slow", b, 0)
	loser.QueueWrite(Frame{Addr: 0x11, Data: []byte{0x11}})
	winner := NewMaster("fast", b, 0)
	winner.QueueWrite(Frame{Addr: 0x10, Data: []byte{0x22}})
	slave10 := NewSlave("s10", 0x10)
	slave11 := NewSlave("s11", 0x11)
	r := NewRunner(b, loser, winner, NewArbiter("arb", loser, winner), slave10, slave11)
	first := waitDone(t, loser.Done(), 100, r)
	if !loser.Lost() || first.Err == nil || !strings.Contains(first.Err.Error(), "lost arbitration") {
		t.Fatalf("loser result = %v, want arbitration loss", first.Err)
	}
	if loser.LostAt() != 34 {
		t.Fatalf("lost at %d, want 34", loser.LostAt())
	}
	if got := loser.PendingFrames(); len(got) != 1 || got[0].Addr != 0x11 || len(got[0].Data) != 1 {
		t.Fatalf("retry evidence = %+v, want original frame", got)
	}
	waitDone(t, winner.Done(), 100, r)
	if winner.Result().Err != nil {
		t.Fatalf("winner error: %v", winner.Result().Err)
	}
	if got := slave10.Bytes(); len(got) != 1 || got[0] != 0x22 {
		t.Fatalf("winner delivered %v, want 22", got)
	}
	if !loser.CanRetryAt(loser.EarliestRetry()) || loser.EarliestRetry() != 85 {
		t.Fatalf("earliest retry = %d, want 85", loser.EarliestRetry())
	}
	if loser.CanRetryAt(84) {
		t.Fatal("retry allowed before an idle bit period")
	}
	if !loser.Retry(85) {
		t.Fatal("retry at earliest idle boundary rejected")
	}
	waitDone(t, loser.Done(), 100, r)
	if loser.Result().Err != nil {
		t.Fatalf("retried transaction: %v", loser.Result().Err)
	}
	if got := slave11.Bytes(); len(got) != 1 || got[0] != 0x11 {
		t.Fatalf("retried delivery = %v, want 11", got)
	}
	if !b.Idle() {
		t.Fatal("bus did not return idle after retry")
	}
}

type holdingDevice struct {
	id        string
	releaseAt int
}

func (h *holdingDevice) ID() string { return h.id }
func (h *holdingDevice) Drive(tick, phase int) Drive {
	if h.releaseAt > 0 && tick >= h.releaseAt {
		return Release
	}
	return Drive{SCL: High, SDA: Low}
}
func (h *holdingDevice) Observe(Snapshot) {}
func (h *holdingDevice) OnEvent(kind, who, detail string) {
	if kind == EvRecoverClock && detail == "pulse=9" {
		h.releaseAt = 55
	}
}

func TestResetAndNineClockRecoveryReturnsBusToIdle(t *testing.T) {
	b := New()
	master := NewMaster("m", b, 0)
	master.QueueWrite(Frame{Addr: 0x10, Data: []byte{0x01}})
	holder := &holdingDevice{id: "holder"}
	recovery := NewRecovery("recover", 17)
	arbiter := NewArbiter("arb", master)
	arbiter.SuppressRecovery(recovery)
	r := NewRunner(b, master, holder, recovery, arbiter)

	r.Run(60)
	if !recovery.Done() {
		t.Fatal("recovery did not finish")
	}
	if master.Active() || master.Result().Err == nil {
		t.Fatalf("master state active=%v err=%v, want aborted", master.Active(), master.Result().Err)
	}
	clocks := 0
	sawReset := false
	for _, event := range eventKinds(b) {
		if strings.HasPrefix(event, EvRecoverClock+":") {
			clocks++
		}
		if event == EvReset+":recover" {
			sawReset = true
		}
	}
	if !sawReset || clocks != 9 {
		t.Fatalf("reset=%v clocks=%d, want reset and 9 pulses", sawReset, clocks)
	}
	if !b.Idle() {
		t.Fatalf("bus = (%s,%s), want recovered idle", b.scl, b.sda)
	}
}
