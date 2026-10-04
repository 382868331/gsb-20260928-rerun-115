package bus

import (
	"reflect"
	"strings"
	"testing"
)

// 组传输：主机获得线路后连续完成写-读-写三个操作，中间保持占用；
// 从设备在读操作中途暂停、接续，并在第三次被寻址时退出。
func TestGroupHoldsBusWithSlavePauseResumeAndQuit(t *testing.T) {
	b := New()
	master := NewMaster("g0", b, 0)
	master.QueueGroup(
		Op{Addr: 0x10, Write: []byte{0xAA}},
		Op{Addr: 0x10, Read: 2},
		Op{Addr: 0x10, Write: []byte{0xCC}},
	)
	slave := NewSlave("s10", 0x10)
	slave.SetScript(SlaveScript{
		ReadData:    []byte{0x0F, 0xF0},
		StretchBit:  3,
		StretchBits: 2,
		QuitAtAddr:  2,
	})
	witness := NewManualDevice("witness", nil)
	r := NewRunner(b, master, slave, NewArbiter("arb", master), witness)

	sawWaitingDevice := false
	sawDriving := false
	var result TxResult
	for i := 0; i < 600; i++ {
		select {
		case result = <-master.Done():
			goto done
		default:
		}
		if master.Status() == StatusWaitingDevice {
			sawWaitingDevice = true
		}
		if master.Status() == StatusDriving {
			sawDriving = true
		}
		r.Step()
	}
	t.Fatal("group did not finish")
done:
	if !sawWaitingDevice || !sawDriving {
		t.Fatalf("status waiting-device=%v driving=%v, want both observed", sawWaitingDevice, sawDriving)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "NACK") {
		t.Fatalf("group err = %v, want NACK after slave quit", result.Err)
	}
	if len(result.Ops) != 3 {
		t.Fatalf("ops = %d, want 3", len(result.Ops))
	}
	if !result.Ops[0].Acked || result.Ops[0].Err != nil {
		t.Fatalf("op0 = %+v, want confirmed write", result.Ops[0])
	}
	if !result.Ops[1].Acked || !reflect.DeepEqual(result.Ops[1].Data, []byte{0x0F, 0xF0}) {
		t.Fatalf("op1 = %+v, want confirmed read [0F F0]", result.Ops[1])
	}
	if result.Ops[2].Acked || result.Ops[2].Err == nil {
		t.Fatalf("op2 = %+v, want failed after slave quit", result.Ops[2])
	}
	if got := slave.Bytes(); !reflect.DeepEqual(got, []byte{0xAA}) {
		t.Fatalf("slave got %v, want [AA]", got)
	}
	if !slave.Detached() {
		t.Fatal("slave did not detach after quit")
	}
	for _, d := range b.Devices() {
		if d.ID() == "s10" {
			t.Fatal("detached slave still on bus")
		}
	}
	if !b.Idle() {
		t.Fatalf("bus = (%s,%s), want idle", b.scl, b.sda)
	}
	// 组内只有重复 START，没有中途 STOP；组末一次 STOP；线路归属始终
	// 是本组主机或目标从设备，没有别人插入。
	starts, stops := 0, 0
	inGroup := false
	for _, snap := range witness.Snapshots() {
		if snap.StartCondition() {
			starts++
			inGroup = true
		}
		if snap.StopCondition() {
			stops++
			inGroup = false
		}
		if inGroup {
			for _, owner := range append(snap.OwnersSCL, snap.OwnersSDA...) {
				if owner != "g0" && owner != "s10" {
					t.Fatalf("tick %d: line owned by %s inside group", snap.Tick, owner)
				}
			}
		}
	}
	if starts != 3 || stops != 1 {
		t.Fatalf("START=%d STOP=%d, want 3 START and 1 STOP", starts, stops)
	}
}

// 两组操作争用：失败方保留已确认结果，重试时不重发已确认的写入。
func TestGroupContentionLoserResumesWithoutResending(t *testing.T) {
	b := New()
	winner := NewMaster("w", b, 0)
	winner.QueueGroup(
		Op{Addr: 0x10, Write: []byte{0xAA}},
		Op{Addr: 0x10, Write: []byte{0xBB}},
	)
	loser := NewMaster("l", b, 0)
	loser.QueueGroup(
		Op{Addr: 0x10, Write: []byte{0xAA}},
		Op{Addr: 0x10, Write: []byte{0xBF}},
	)
	slave := NewSlave("s10", 0x10)
	r := NewRunner(b, winner, loser, NewArbiter("arb", winner, loser), slave)

	first := waitDone(t, loser.Done(), 400, r)
	if !loser.Lost() || loser.Status() != StatusLost {
		t.Fatalf("loser status = %v lost=%v, want lost-occupancy", loser.Status(), loser.Lost())
	}
	if first.Err == nil || !strings.Contains(first.Err.Error(), "lost arbitration") {
		t.Fatalf("loser first result = %v, want arbitration loss", first.Err)
	}
	// 第一个操作与胜方完全一致，已被从设备确认；失败发生在第二个操作。
	if len(first.Ops) != 2 || !first.Ops[0].Acked {
		t.Fatalf("loser ops = %+v, want op0 confirmed", first.Ops)
	}
	if first.Ops[1].Err == nil {
		t.Fatalf("loser op1 = %+v, want failed", first.Ops[1])
	}
	waitDone(t, winner.Done(), 400, r)
	if winner.Result().Err != nil {
		t.Fatalf("winner error: %v", winner.Result().Err)
	}
	for b.Now < loser.EarliestRetry() {
		r.Step()
	}
	if !loser.Retry(loser.EarliestRetry()) {
		t.Fatal("retry at earliest idle boundary rejected")
	}
	waitDone(t, loser.Done(), 400, r)
	if loser.Result().Err != nil {
		t.Fatalf("retried group: %v", loser.Result().Err)
	}
	// 0xAA 只出现一次：已确认的写入没有为重发追平进度而重复。
	want := []byte{0xAA, 0xBB, 0xBF}
	if got := slave.Bytes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("slave got %v, want %v (no resent confirmed write)", got, want)
	}
	if !b.Idle() {
		t.Fatal("bus did not return idle")
	}
}

// 异常处置请求：abort 在当前操作边界中止本组；wait 等待本组完成。
func TestExceptionAbortAndWait(t *testing.T) {
	newGroup := func() (*Bus, *Master, *Slave, *Runner) {
		b := New()
		master := NewMaster("g", b, 0)
		master.QueueGroup(
			Op{Addr: 0x10, Write: []byte{1}},
			Op{Addr: 0x10, Write: []byte{2}},
			Op{Addr: 0x10, Write: []byte{3}},
		)
		slave := NewSlave("s10", 0x10)
		r := NewRunner(b, master, slave, NewArbiter("arb", master))
		return b, master, slave, r
	}

	t.Run("abort", func(t *testing.T) {
		b, master, slave, r := newGroup()
		for b.Now < 100 {
			r.Step()
		}
		b.Emit(EvException, "supervisor", "abort")
		result := waitDone(t, master.Done(), 400, r)
		if result.Err == nil || !strings.Contains(result.Err.Error(), "exception") {
			t.Fatalf("result = %v, want exception abort", result.Err)
		}
		if !result.Ops[0].Acked || !result.Ops[1].Acked {
			t.Fatalf("ops = %+v, want first two ops confirmed", result.Ops)
		}
		if result.Ops[2].Err == nil {
			t.Fatalf("op2 = %+v, want dropped", result.Ops[2])
		}
		if got := slave.Bytes(); !reflect.DeepEqual(got, []byte{1, 2}) {
			t.Fatalf("slave got %v, want [1 2]", got)
		}
		if !b.Idle() {
			t.Fatal("bus not idle after exception abort")
		}
	})

	t.Run("wait", func(t *testing.T) {
		b, master, slave, r := newGroup()
		for b.Now < 100 {
			r.Step()
		}
		b.Emit(EvException, "supervisor", "wait")
		result := waitDone(t, master.Done(), 400, r)
		if result.Err != nil {
			t.Fatalf("result = %v, want clean finish under wait", result.Err)
		}
		if !master.ExceptionPending() {
			t.Fatal("exception-wait not recorded")
		}
		if got := slave.Bytes(); !reflect.DeepEqual(got, []byte{1, 2, 3}) {
			t.Fatalf("slave got %v, want [1 2 3]", got)
		}
		if !b.Idle() {
			t.Fatal("bus not idle after group")
		}
	})
}

// 在组内任意一位保存后继续：波形与无保存运行一致，已完成字节与后续仲裁
// 一致；人工放弃剩余操作后，等待线路的另一主机取得总线。
func TestCheckpointContinueAndManualAbortHandoff(t *testing.T) {
	scenario := func(takeCheckpoint bool, cp *Checkpoint) []Entry {
		b := New()
		g1 := NewMaster("g1", b, 0)
		g1.QueueGroup(
			Op{Addr: 0x10, Write: []byte{1}},
			Op{Addr: 0x10, Write: []byte{2}},
			Op{Addr: 0x10, Write: []byte{3}},
		)
		g2 := NewMaster("g2", b, 8)
		g2.QueueGroup(Op{Addr: 0x11, Write: []byte{9}})
		s10 := NewSlave("s10", 0x10)
		s11 := NewSlave("s11", 0x11)
		r := NewRunner(b, g1, g2, NewArbiter("arb", g1, g2), s10, s11)

		g2Waited := false
		for b.Now < 100 {
			if g2.Status() == StatusWaitingBus {
				g2Waited = true
			}
			r.Step()
		}
		if takeCheckpoint {
			// 组内任意一位（此处为 op1 数据位中途）保存。
			*cp = r.Checkpoint()
		}
		for b.Now < 120 {
			r.Step()
		}
		g1.AbortGroup("manual: give up remaining ops")
		waitDone(t, g1.Done(), 400, r)
		waitDone(t, g2.Done(), 400, r)

		if !g2Waited {
			t.Fatal("g2 never reported waiting-bus while g1 held the line")
		}
		if g2.Result().Err != nil {
			t.Fatalf("g2 error: %v", g2.Result().Err)
		}
		if got := s10.Bytes(); !reflect.DeepEqual(got, []byte{1, 2}) {
			t.Fatalf("s10 got %v, want [1 2]", got)
		}
		if got := s11.Bytes(); !reflect.DeepEqual(got, []byte{9}) {
			t.Fatalf("s11 got %v, want [9]", got)
		}
		if !b.Idle() {
			t.Fatal("bus not idle at end")
		}
		// g2 的授权只能出现在 g1 发出 STOP 之后。
		stopTick, grantTick := -1, -1
		for _, e := range b.Trace {
			if e.Kind == "level" && stopTick < 0 && e.Tick > 120 && e.SCL == High && e.SDA == High {
				stopTick = e.Tick
			}
			if e.Kind == EvArbGrant && e.Who == "g2" {
				grantTick = e.Tick
			}
		}
		if stopTick < 0 || grantTick < 0 || grantTick < stopTick {
			t.Fatalf("stop=%d grant=%d, want g2 granted after g1 STOP", stopTick, grantTick)
		}
		return b.Trace
	}

	var cp Checkpoint
	traceWith := scenario(true, &cp)
	traceWithout := scenario(false, nil)
	if !reflect.DeepEqual(traceWith, traceWithout) {
		t.Fatal("waveform diverged after checkpoint save; save must be non-intrusive")
	}
	if cp.Tick != 100 {
		t.Fatalf("checkpoint tick = %d, want 100", cp.Tick)
	}
	if cp.Confirmed["g1"] != 1 {
		t.Fatalf("checkpoint confirmed = %d, want 1 op done", cp.Confirmed["g1"])
	}
	if cp.SlaveBytes["s10"] != 1 {
		t.Fatalf("checkpoint slave bytes = %d, want 1", cp.SlaveBytes["s10"])
	}
}
