package bus

import "testing"

func TestSlaveFaultClearsAndTransferResumesWithoutBitLoss(t *testing.T) {
	normalBus, normalRunner, normalMaster, _, _ := setupFaultBus(t, SlaveFault{}, 0)
	waitDone(t, normalMaster.Done(), 100, normalRunner)

	fault := SlaveFault{StretchAt: 22, StretchTicks: 4, HoldAt: 22, HoldTicks: 4, ClearAt: 26}
	faultBus, faultRunner, faultMaster, faultSlave, witness := setupFaultBus(t, fault, 0)
	result := waitDone(t, faultMaster.Done(), 120, faultRunner)
	if result.Err != nil {
		t.Fatalf("transfer after fault clear: %v", result.Err)
	}
	if got := faultSlave.Bytes(); len(got) != 1 || got[0] != 0xA5 {
		t.Fatalf("slave got %v after clear, want A5", got)
	}
	if !faultSlave.FaultCleared() || !faultBus.Idle() {
		t.Fatalf("cleared=%v idle=%v, want fault cleared and idle bus", faultSlave.FaultCleared(), faultBus.Idle())
	}
	if faultBus.Now <= normalBus.Now {
		t.Fatalf("fault-clear run %d should exceed normal run %d", faultBus.Now, normalBus.Now)
	}
	for _, snap := range witness.Snapshots() {
		if snap.Tick >= 22 && snap.Tick < 26 && snap.SCL == High {
			t.Fatal("line completed a clock pulse during the held fault window")
		}
	}
}
