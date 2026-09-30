package bus

import "testing"

func TestRepeatedStartBetweenFrames(t *testing.T) {
	b := New()
	master := NewMaster("m", b, 0)
	master.QueueWrite(
		Frame{Addr: 0x20, Data: []byte{0x01}},
		Frame{Addr: 0x20, Data: []byte{0x02}},
	)
	slave := NewSlave("s20", 0x20)
	witness := NewManualDevice("witness", nil)
	r := NewRunner(b, master, slave, NewArbiter("arb", master), witness)

	result := waitDone(t, master.Done(), 180, r)
	if result.Err != nil {
		t.Fatalf("repeated-start transfer: %v", result.Err)
	}
	starts := 0
	stops := 0
	for _, snap := range witness.Snapshots() {
		if snap.StartCondition() {
			starts++
		}
		if snap.StopCondition() {
			stops++
		}
	}
	if starts != 2 || stops != 1 {
		t.Fatalf("START=%d STOP=%d, want 2 START and 1 STOP", starts, stops)
	}
	if got := slave.Bytes(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("slave got %v, want [1 2]", got)
	}
}
