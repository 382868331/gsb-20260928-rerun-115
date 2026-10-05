package bus

import (
	"strings"
	"testing"
)

func waitGroupDone(t *testing.T, ch <-chan GroupResult, ticks int, r *Runner) GroupResult {
	t.Helper()
	for i := 0; i < ticks; i++ {
		select {
		case result := <-ch:
			return result
		default:
		}
		r.Step()
	}
	t.Fatal("group did not finish")
	return GroupResult{}
}

// 组内写-读-写连续完成，中间保持占用（只有重复 START），每次操作保留独立结果。
func TestGroupWriteReadWriteHoldsBusAndKeepsPerOpResults(t *testing.T) {
	b := New()
	g := NewGroupMaster("g", b, 0)
	g.QueueGroup(
		Op{Addr: 0x50, Data: []byte{0x09}},
		Op{Addr: 0x50, Read: true, Len: 3},
		Op{Addr: 0x50, Data: []byte{0x77}},
	)
	slave := NewStretchSlave("s50", 0x50)
	slave.SetReadData([]byte{0xDE, 0xAD, 0xBE})
	witness := NewManualDevice("witness", nil)
	r := NewRunner(b, g, slave, NewArbiterOf("arb", g), witness)

	occupiedSeen := false
	var result GroupResult
	for i := 0; i < 700; i++ {
		select {
		case result = <-g.Done():
			goto done
		default:
		}
		if g.Status() == StatusOccupied {
			occupiedSeen = true
		}
		r.Step()
	}
	t.Fatal("group did not finish")
done:
	if !occupiedSeen {
		t.Fatal("master never reported occupied")
	}
	if result.Err != nil {
		t.Fatalf("group error: %v", result.Err)
	}
	if !result.Ops[0].Done || result.Ops[0].Written != 1 {
		t.Fatalf("op0 = %+v, want 1 written byte done", result.Ops[0])
	}
	if !result.Ops[1].Done || len(result.Ops[1].Read) != 3 ||
		result.Ops[1].Read[0] != 0xDE || result.Ops[1].Read[1] != 0xAD || result.Ops[1].Read[2] != 0xBE {
		t.Fatalf("op1 read = %v, want [DE AD BE]", result.Ops[1].Read)
	}
	if !result.Ops[2].Done || result.Ops[2].Written != 1 {
		t.Fatalf("op2 = %+v, want 1 written byte done", result.Ops[2])
	}
	if got := slave.Bytes(); len(got) != 2 || got[0] != 0x09 || got[1] != 0x77 {
		t.Fatalf("slave got %v, want [09 77]", got)
	}
	starts, stops := 0, 0
	for _, snap := range witness.Snapshots() {
		if snap.StartCondition() {
			starts++
		}
		if snap.StopCondition() {
			stops++
		}
	}
	if starts != 3 || stops != 1 {
		t.Fatalf("START=%d STOP=%d, want 3 START and 1 STOP (bus held across group)", starts, stops)
	}
	if !b.Idle() {
		t.Fatal("bus not idle after group")
	}
}

// 从设备在组中途暂停（拉伸时钟）后再接续；主设备报告“等待设备”。
func TestGroupSlavePauseResumeReportsWaitingDevice(t *testing.T) {
	b := New()
	g := NewGroupMaster("g", b, 0)
	g.QueueGroup(Op{Addr: 0x40, Data: []byte{0x01, 0x02, 0x03}})
	slave := NewStretchSlave("s40", 0x40)
	slave.PauseAfterByte(0, 6)
	r := NewRunner(b, g, slave, NewArbiterOf("arb", g))

	waitingSeen := false
	var result GroupResult
	for i := 0; i < 400; i++ {
		select {
		case result = <-g.Done():
			goto done
		default:
		}
		if g.Status() == StatusWaitingDevice {
			waitingSeen = true
		}
		r.Step()
	}
	t.Fatal("group did not finish")
done:
	if !waitingSeen || !g.SawStretch() {
		t.Fatalf("waiting-device status never observed (sawStretch=%v)", g.SawStretch())
	}
	if result.Err != nil || !result.Ops[0].Done || result.Ops[0].Written != 3 {
		t.Fatalf("result = %+v, want 3 bytes written after resume", result)
	}
	if got := slave.Bytes(); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("slave got %v, want [1 2 3]", got)
	}
	if !b.Idle() {
		t.Fatal("bus not idle after stretched group")
	}
}

// 从设备最终退出（停止应答）：整组以 STOP 结束，已完成操作的结果不被抹掉。
func TestGroupSlaveQuitKeepsPartialResults(t *testing.T) {
	b := New()
	g := NewGroupMaster("g", b, 0)
	g.QueueGroup(
		Op{Addr: 0x40, Data: []byte{0x01, 0x02}},
		Op{Addr: 0x40, Data: []byte{0x03, 0x04}},
	)
	slave := NewStretchSlave("s40", 0x40)
	slave.QuitAfterBytes(3)
	witness := NewManualDevice("witness", nil)
	r := NewRunner(b, g, slave, NewArbiterOf("arb", g), witness)

	result := waitGroupDone(t, g.Done(), 400, r)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "NACK") {
		t.Fatalf("group err = %v, want NACK from quitting device", result.Err)
	}
	if !result.Ops[0].Done || result.Ops[0].Written != 2 || result.Ops[0].Err != nil {
		t.Fatalf("op0 = %+v, want completed with 2 bytes", result.Ops[0])
	}
	if result.Ops[1].Done || result.Ops[1].Written != 1 || result.Ops[1].Err == nil {
		t.Fatalf("op1 = %+v, want 1 acked byte and NACK error", result.Ops[1])
	}
	if got := slave.Bytes(); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("slave got %v, want [1 2 3]", got)
	}
	stops := 0
	for _, snap := range witness.Snapshots() {
		if snap.StopCondition() {
			stops++
		}
	}
	if stops != 1 || !b.Idle() {
		t.Fatalf("stops=%d idle=%v, want single STOP and idle bus", stops, b.Idle())
	}
}

// 两组操作争用：失败方保留已确认进度，续传时不重发已确认的写入。
func TestGroupContentionLoserDoesNotResendAckedBytes(t *testing.T) {
	b := New()
	winner := NewGroupMaster("winner", b, 0)
	winner.QueueGroup(Op{Addr: 0x30, Data: []byte{0xAA, 0x00}})
	loser := NewGroupMaster("loser", b, 0)
	loser.QueueGroup(Op{Addr: 0x30, Data: []byte{0xAA, 0x01}})
	slave := NewSlave("s30", 0x30)
	r := NewRunner(b, winner, loser, NewArbiterOf("arb", winner, loser), slave)

	first := waitGroupDone(t, loser.Done(), 300, r)
	if loser.Status() != StatusLost || first.Err == nil ||
		!strings.Contains(first.Err.Error(), "lost arbitration") {
		t.Fatalf("loser status=%v err=%v, want lost arbitration", loser.Status(), first.Err)
	}
	if acked := first.Ops[0].Written; acked != 1 {
		t.Fatalf("loser acked = %d at loss, want 1 (0xAA confirmed before divergence)", acked)
	}
	waitGroupDone(t, winner.Done(), 300, r)
	if got := slave.Bytes(); len(got) != 2 || got[0] != 0xAA || got[1] != 0x00 {
		t.Fatalf("after winner: slave got %v, want [AA 00]", got)
	}
	if !loser.Retry(loser.EarliestRetry()) {
		t.Fatal("loser retry at earliest idle boundary rejected")
	}
	resumed := waitGroupDone(t, loser.Done(), 300, r)
	if resumed.Err != nil || resumed.Ops[0].Written != 2 {
		t.Fatalf("resumed = %+v, want 2 total acked bytes", resumed)
	}
	got := slave.Bytes()
	if len(got) != 3 || got[0] != 0xAA || got[1] != 0x00 || got[2] != 0x01 {
		t.Fatalf("slave got %v, want [AA 00 01] (no resent 0xAA)", got)
	}
	if !b.Idle() {
		t.Fatal("bus not idle after contention resolved")
	}
}

// 人工放弃剩余操作后，等待线路的高优先级主设备取得线路并完成自己的组。
func TestGroupInterruptReleasesBusForWaitingMaster(t *testing.T) {
	b := New()
	g := NewGroupMaster("g", b, 0)
	g.QueueGroup(
		Op{Addr: 0x60, Data: []byte{0x01}},
		Op{Addr: 0x60, Data: []byte{0x02}},
		Op{Addr: 0x60, Data: []byte{0x03}},
		Op{Addr: 0x60, Data: []byte{0x04}},
	)
	s60 := NewSlave("s60", 0x60)
	s61 := NewSlave("s61", 0x61)
	witness := NewManualDevice("witness", nil)
	arb := NewArbiterOf("arb", g)
	r := NewRunner(b, g, arb, s60, s61, witness)

	interrupted := false
	var gres GroupResult
	for i := 0; i < 800; i++ {
		if !interrupted && g.Result().Ops[1].Done {
			g.Interrupt("manual abort of remaining ops")
			interrupted = true
		}
		if !interrupted {
			r.Step()
			continue
		}
		select {
		case gres = <-g.Done():
			goto gdone
		default:
		}
		r.Step()
	}
	t.Fatal("group did not finish after interrupt")
gdone:
	if !gres.Aborted || gres.Err == nil {
		t.Fatalf("group = %+v, want aborted", gres)
	}
	if !gres.Ops[0].Done || !gres.Ops[1].Done || gres.Ops[2].Done || gres.Ops[3].Done {
		t.Fatalf("ops = %+v, want first two done, rest dropped", gres.Ops)
	}
	if got := s60.Bytes(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("s60 got %v, want [1 2]", got)
	}

	// 高优先级异常处置请求此刻才到达：先等待线路，再取得占用。
	hp := NewGroupMaster("hp", b, -1) // 等待线路
	hp.QueueGroup(Op{Addr: 0x61, Data: []byte{0xEE}})
	arb.Watch(hp)
	r.Add(hp)
	if hp.Status() != StatusWaitingBus {
		t.Fatalf("hp status = %v, want waiting-bus", hp.Status())
	}
	hres := waitGroupDone(t, hp.Done(), 300, r)
	if hres.Err != nil || !hres.Ops[0].Done {
		t.Fatalf("hp = %+v, want completed", hres)
	}
	if got := s61.Bytes(); len(got) != 1 || got[0] != 0xEE {
		t.Fatalf("s61 got %v, want [EE]", got)
	}

	// 波形核对：hp 的 START 必须出现在 g 的 STOP 之后，且 START 只有一个占有者。
	gStop, hpStart := -1, -1
	for _, snap := range witness.Snapshots() {
		if snap.StopCondition() && gStop < 0 && snap.Tick > 10 {
			gStop = snap.Tick
		}
		if snap.StartCondition() && len(snap.OwnersSDA) == 1 && snap.OwnersSDA[0] == "hp" {
			hpStart = snap.Tick
		}
		if snap.StartCondition() && len(snap.OwnersSDA) != 1 {
			t.Fatalf("START at tick %d has owners %v, want exactly one", snap.Tick, snap.OwnersSDA)
		}
	}
	if gStop < 0 || hpStart < 0 || hpStart <= gStop {
		t.Fatalf("g STOP at %d, hp START at %d, want hp strictly after", gStop, hpStart)
	}
	if !b.Idle() {
		t.Fatal("bus not idle at end")
	}
}

// 在组内任意一位保存，中止后继续：已完成字节保留、波形与后续仲裁一致。
func TestGroupSaveResumeAtArbitraryBit(t *testing.T) {
	b := New()
	g := NewGroupMaster("g", b, 0)
	g.QueueGroup(Op{Addr: 0x70, Data: []byte{0x10, 0x11, 0x12, 0x13}})
	s70 := NewSlave("s70", 0x70)
	s71 := NewSlave("s71", 0x71)
	witness := NewManualDevice("witness", nil)
	r := NewRunner(b, g, s70, s71, witness)

	// 推进到第 3 字节中途（任意一位）再保存。
	for g.Result().Ops[0].Written < 2 {
		r.Step()
	}
	r.Step()
	r.Step()
	r.Step()
	cp := g.Save()
	if cp.Results[0].Written != 2 {
		t.Fatalf("checkpoint written = %d, want 2", cp.Results[0].Written)
	}
	b.Emit(EvReset, "operator", "save-and-abort")
	r.Run(8)
	if g.Status() != StatusAborted {
		t.Fatalf("status = %v, want aborted after reset", g.Status())
	}

	// 继续时与另一组操作同时争线，核对仲裁一致性。
	resumeTick := b.Now + 8
	g.ResumeAt(cp, resumeTick)
	h := NewGroupMaster("h", b, resumeTick)
	h.QueueGroup(Op{Addr: 0x71, Data: []byte{0x99}})
	arb := NewArbiterOf("arb", g, h)
	r.Add(h, arb)

	sawLost := false
	for i := 0; i < 900 && (g.Status() != StatusDone || h.Status() != StatusDone); i++ {
		r.Step()
		if g.Status() == StatusLost && g.CanRetryAt(b.Now) {
			sawLost = true
			g.Retry(g.EarliestRetry())
		}
		if h.Status() == StatusLost && h.CanRetryAt(b.Now) {
			sawLost = true
			h.Retry(h.EarliestRetry())
		}
	}
	if g.Status() != StatusDone || h.Status() != StatusDone {
		t.Fatalf("g=%v h=%v, want both done", g.Status(), h.Status())
	}
	if !sawLost {
		t.Fatal("expected one master to lose arbitration after resume")
	}
	if got := s70.Bytes(); len(got) != 4 || got[0] != 0x10 || got[1] != 0x11 || got[2] != 0x12 || got[3] != 0x13 {
		t.Fatalf("s70 got %v, want [10 11 12 13] (no duplicates after resume)", got)
	}
	if got := s71.Bytes(); len(got) != 1 || got[0] != 0x99 {
		t.Fatalf("s71 got %v, want [99]", got)
	}
	gres := g.Result()
	if gres.Err != nil || gres.Ops[0].Written != 4 {
		t.Fatalf("g result = %+v, want 4 acked bytes", gres)
	}
	// 仲裁一致性：输掉的一方从 arb-lost 拍起完全释放线路，
	// 直到总线空闲后才以新的 START 重新取得占用。
	loserID := ""
	lostTick := -1
	for _, entry := range b.Trace {
		if entry.Kind == EvArbLost {
			loserID = entry.Who
			lostTick = entry.Tick
		}
	}
	if loserID == "" {
		t.Fatal("no arb-lost event recorded")
	}
	for _, snap := range witness.Snapshots() {
		if snap.Tick <= lostTick {
			continue
		}
		for _, owner := range snap.OwnersSDA {
			if owner == loserID && len(snap.OwnersSDA) > 1 {
				t.Fatalf("loser %s still driving SDA at tick %d with %v", loserID, snap.Tick, snap.OwnersSDA)
			}
		}
	}
	for _, snap := range witness.Snapshots() {
		if snap.StartCondition() {
			for _, owner := range snap.OwnersSDA {
				if owner != "g" && owner != "h" {
					t.Fatalf("START at tick %d owned by %v, want only masters", snap.Tick, snap.OwnersSDA)
				}
			}
		}
	}
	if !b.Idle() {
		t.Fatal("bus not idle at end")
	}
}

// 从设备在重复 START 前拉伸时钟：位位置必须保持，重复 START 后的寻址与
// 读写方向切换不能错位，读回字节必须与预置一致，且读地址/读数据不得
// 混进从设备的写入记录。
func TestGroupStretchBeforeRepeatedStartKeepsBitPosition(t *testing.T) {
	b := New()
	g := NewGroupMaster("g", b, 0)
	g.QueueGroup(
		Op{Addr: 0x50, Data: []byte{0x11, 0x22}},
		Op{Addr: 0x50, Read: true, Len: 1},
	)
	slave := NewStretchSlave("s50", 0x50)
	slave.SetReadData([]byte{0xA5})
	slave.PauseAfterByte(1, 6)
	r := NewRunner(b, g, slave, NewArbiterOf("arb", g))

	waitingSeen := false
	var result GroupResult
	for i := 0; i < 700; i++ {
		select {
		case result = <-g.Done():
			goto done
		default:
		}
		if g.Status() == StatusWaitingDevice {
			waitingSeen = true
		}
		r.Step()
	}
	t.Fatal("group did not finish")
done:
	if !waitingSeen || !g.SawStretch() {
		t.Fatalf("stretch before repeated START not observed (waiting=%v saw=%v)", waitingSeen, g.SawStretch())
	}
	if result.Err != nil {
		t.Fatalf("group error: %v", result.Err)
	}
	if !result.Ops[0].Done || result.Ops[0].Written != 2 {
		t.Fatalf("op0 = %+v, want 2 written bytes done", result.Ops[0])
	}
	if !result.Ops[1].Done || len(result.Ops[1].Read) != 1 || result.Ops[1].Read[0] != 0xA5 {
		t.Fatalf("op1 read = % x, want [A5]", result.Ops[1].Read)
	}
	if got := slave.Bytes(); len(got) != 2 || got[0] != 0x11 || got[1] != 0x22 {
		t.Fatalf("slave got % x, want [11 22] (no read address/data mixed in)", got)
	}
	if !b.Idle() {
		t.Fatal("bus not idle after group")
	}
}

// 从设备在最后一字节的 ACK 之后拉伸时钟（下一段是 STOP）：主设备同样
// 必须顺延，STOP 之后总线空闲且结果完整。
func TestGroupStretchBeforeStop(t *testing.T) {
	b := New()
	g := NewGroupMaster("g", b, 0)
	g.QueueGroup(Op{Addr: 0x40, Data: []byte{0x5A}})
	slave := NewStretchSlave("s40", 0x40)
	slave.PauseAfterByte(0, 5)
	r := NewRunner(b, g, slave, NewArbiterOf("arb", g))

	result := waitGroupDone(t, g.Done(), 400, r)
	if result.Err != nil || !result.Ops[0].Done || result.Ops[0].Written != 1 {
		t.Fatalf("result = %+v, want 1 byte written after stretched STOP", result)
	}
	if got := slave.Bytes(); len(got) != 1 || got[0] != 0x5A {
		t.Fatalf("slave got % x, want [5A]", got)
	}
	if !b.Idle() {
		t.Fatal("bus not idle after stretched STOP")
	}
}
