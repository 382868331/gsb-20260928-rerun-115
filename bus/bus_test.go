package bus

import (
	"errors"
	"testing"
)

func waitResult(m *Master) TxResult {
	return <-m.Done()
}

// 1) 基本写：START + 地址 ACK + 数据 ACK + STOP，从设备收到字节。
func TestBasicWrite(t *testing.T) {
	b := New()
	r := NewRunner(b)
	sl := NewSlave("sl", 0x48)
	r.Attach(sl)

	m := NewMaster("m1", b, 0)
	m.QueueWrite(Frame{Addr: 0x48, Data: []byte{0xA5}})
	r.Master(m)

	r.Run(200)
	res := waitResult(m)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if got := sl.Bytes(); len(got) != 1 || got[0] != 0xA5 {
		t.Fatalf("slave got %v, want [A5]", got)
	}
	if !b.Idle() {
		t.Fatalf("bus not idle after STOP: SCL=%v SDA=%v", b.scl, b.sda)
	}
}

// 2) 两主设备同步发起，地址字节仲裁：发 1 的一方输掉并立即释放。
func TestArbitrationTwoMasters(t *testing.T) {
	b := New()
	r := NewRunner(b)
	r.Attach(NewSlave("sl01", 0x01))
	r.Attach(NewSlave("sl03", 0x03))

	// 地址 0x01+W = 0b00000010，第二位即出现 0 vs 1；地址 0x03+W=0b00000110。
	mLow := NewMaster("mA", b, 0)
	mLow.QueueWrite(Frame{Addr: 0x01, Data: []byte{0x00}})
	r.Master(mLow)

	mHigh := NewMaster("mB", b, 0)
	mHigh.QueueWrite(Frame{Addr: 0x03, Data: []byte{0x00}})
	r.Master(mHigh)

	r.Run(400)

	loser := waitResult(mHigh)
	if !errors.Is(loser.Err, ErrArbLost) {
		t.Fatalf("mB err = %v, want ErrArbLost", loser.Err)
	}
	if mHigh.Active() || mHigh.Granted() {
		t.Fatal("loser must not be active/granted")
	}
	if !mHigh.Lost() {
		t.Fatal("loser must retain lost state for retry decision")
	}

	winner := waitResult(mLow)
	if winner.Err != nil {
		t.Fatalf("winner unexpected err: %v", winner.Err)
	}
	if !mLow.Granted() {
		t.Fatal("winner must be granted")
	}
	if !b.Idle() {
		t.Fatal("bus must be idle after winner's STOP")
	}

	// 同一时刻绝不允许两个设备都认为自己独占总线。
	if mHigh.Granted() && mLow.Granted() {
		t.Fatal("both masters granted")
	}

	// 输方保留事务，可以重新发起并成功。
	if !mHigh.Retry(b.Tick) {
		t.Fatal("retry should be allowed after arbitration loss")
	}
	r.Run(400)
	if res := waitResult(mHigh); res.Err != nil {
		t.Fatalf("retry failed: %v", res.Err)
	}
	if mHigh.Lost() {
		t.Fatal("Lost flag must clear after successful retry")
	}
}

// 3) 无响应：地址无 ACK，主设备发 NACK 后补 STOP，不永久等待。
func TestNoResponseNACK(t *testing.T) {
	b := New()
	r := NewRunner(b)
	m := NewMaster("m1", b, 0)
	m.QueueWrite(Frame{Addr: 0x77, Data: []byte{1, 2, 3}})
	r.Master(m)

	r.Run(300)
	res := waitResult(m)
	if !errors.Is(res.Err, ErrNACK) {
		t.Fatalf("err = %v, want ErrNACK", res.Err)
	}
	if !b.Idle() {
		t.Fatal("bus must return idle after NACK+STOP")
	}
}

// 4) 等待空闲：总线忙时 QueueWriteWhenIdle 的设备应等待，再在空闲后成功。
func TestWaitUntilIdle(t *testing.T) {
	b := New()
	r := NewRunner(b)
	sl := NewSlave("sl", 0x10)
	r.Attach(sl)

	busy := NewManualDevice("busy")
	// 前 8 个 tick 强行拉低 SDA 模拟总线忙。
	busy.SetDrive(func(tick, phase int) Drive {
		if tick < 8 {
			return Drive{SCL: High, SDA: Low}
		}
		return Release
	})
	r.Attach(busy)

	m := NewMaster("m1", b, 0)
	m.QueueWriteWhenIdle(100, Frame{Addr: 0x10, Data: []byte{0x0F}})
	r.Master(m)

	r.Run(400)
	res := waitResult(m)
	if res.Err != nil {
		t.Fatalf("wait+write failed: %v", res.Err)
	}
	if got := sl.Bytes(); len(got) != 1 || got[0] != 0x0F {
		t.Fatalf("slave got %v", got)
	}

	// 等待超时路径。
	b2 := New()
	r2 := NewRunner(b2)
	hold := NewManualDevice("hold")
	hold.SetDrive(func(tick, phase int) Drive { return Drive{SCL: High, SDA: Low} })
	r2.Attach(hold)
	m2 := NewMaster("m2", b2, 0)
	m2.QueueWriteWhenIdle(8, Frame{Addr: 0x10})
	r2.Master(m2)
	r2.Run(40)
	if res := waitResult(m2); !errors.Is(res.Err, ErrWaitBusy) {
		t.Fatalf("err = %v, want ErrWaitBusy", res.Err)
	}
}

// 5) 重复 START：多帧事务里从设备应在 Sr 后重新匹配地址。
func TestRepeatedStart(t *testing.T) {
	b := New()
	r := NewRunner(b)
	sl := NewSlave("sl", 0x20)
	r.Attach(sl)

	m := NewMaster("m1", b, 0)
	m.QueueWrite(
		Frame{Addr: 0x20, Data: []byte{0x11}},
		Frame{Addr: 0x20, Data: []byte{0x22}},
	)
	r.Master(m)
	r.Run(400)
	if res := waitResult(m); res.Err != nil {
		t.Fatalf("unexpected: %v", res.Err)
	}
	got := sl.Bytes()
	if len(got) != 2 || got[0] != 0x11 || got[1] != 0x22 {
		t.Fatalf("slave got %v, want 11 22", got)
	}
	if !b.Idle() {
		t.Fatal("bus must idle after multi-frame STOP")
	}
}

// 6) 传输中复位：主设备立即中止，从设备释放，线路空闲。
func TestResetInTransit(t *testing.T) {
	b := New()
	r := NewRunner(b)
	sl := NewSlave("sl", 0x30)
	r.Attach(sl)

	m := NewMaster("m1", b, 0)
	m.QueueWrite(Frame{Addr: 0x30, Data: []byte{1, 2, 3}})
	r.Master(m)

	r.Run(14) // 地址传输中途
	r.AbortReset("monitor")
	r.Run(10)

	res := waitResult(m)
	if !errors.Is(res.Err, ErrReset) {
		t.Fatalf("err = %v, want ErrReset", res.Err)
	}
	if m.Active() {
		t.Fatal("master must stop driving after reset")
	}
	if !b.Idle() {
		t.Fatal("healthy devices must release lines after reset")
	}
}

// 7) 故障设备锁死 SDA：9 个恢复时钟后释放，补 STOP，总线恢复空闲。
func TestRecoveryUnlocksSDA(t *testing.T) {
	b := New()
	r := NewRunner(b)
	sl := NewSlave("sl", 0x30)
	r.Attach(sl)

	m := NewMaster("m1", b, 0)
	m.QueueWrite(Frame{Addr: 0x30, Data: []byte{1}})
	r.Master(m)

	// 故障设备：tick>=8 起锁死 SDA，前 3 个恢复时钟（每个 4 tick）后释放。
	bad := NewManualDevice("bad-slave")
	bad.HoldSDAAbort(8, 0) // 先默认一直拉低，下面按脉冲数释放
	releaseTick := 0
	bad.SetDrive(func(tick, phase int) Drive {
		if tick >= 8 && tick < releaseTick {
			return Drive{SCL: High, SDA: Low}
		}
		return Release
	})
	r.Attach(bad)

	r.Run(10)
	releaseTick = b.Tick + 1 + 3*4 // 复位下一拍开始，3 个脉冲后释放
	doneTick := r.Recover("monitor")
	for b.Tick < doneTick+2 {
		r.Step()
	}

	if res := waitResult(m); !errors.Is(res.Err, ErrReset) {
		t.Fatalf("master err = %v, want ErrReset", res.Err)
	}
	if !b.Idle() {
		t.Fatalf("bus must recover to idle, SCL=%v SDA=%v", b.scl, b.sda)
	}
	if !hasEvent(b.Trace, EvRecoverDone) {
		t.Fatalf("missing recovery-done event, trace=%v", b.Trace)
	}

	// 恢复之后可以正常重新发起。
	m2 := NewMaster("m2", b, b.Tick)
	m2.QueueWrite(Frame{Addr: 0x30, Data: []byte{0x5A}})
	r.Master(m2)
	r.Run(300)
	if res := waitResult(m2); res.Err != nil {
		t.Fatalf("post-recovery write failed: %v", res.Err)
	}
}

// 8) 故障设备始终不释放：恢复以 EvRecoverFail 结束，总线仍不可用。
func TestRecoveryFailsWhenLineStuck(t *testing.T) {
	b := New()
	r := NewRunner(b)

	bad := NewManualDevice("bad-slave")
	bad.SetDrive(func(tick, phase int) Drive { return Drive{SCL: High, SDA: Low} })
	r.Attach(bad)

	doneTick := r.Recover("monitor")
	for b.Tick < doneTick+2 {
		r.Step()
	}
	if !hasEvent(b.Trace, EvRecoverFail) {
		t.Fatalf("expected recovery-failed, trace=%v", b.Trace)
	}
	if b.Idle() {
		t.Fatal("bus must NOT be idle while SDA is stuck")
	}
}

// 9) 人工设备逐拍核对：START 边沿、拉线路的设备、仲裁败者释放时刻。
func TestManualPhaseAudit(t *testing.T) {
	b := New()
	r := NewRunner(b)

	mLow := NewMaster("mA", b, 0)
	mLow.QueueWrite(Frame{Addr: 0x01})
	r.Master(mLow)
	mHigh := NewMaster("mB", b, 0)
	mHigh.QueueWrite(Frame{Addr: 0x03})
	r.Master(mHigh)

	probe := NewManualDevice("probe")
	r.Attach(probe)

	r.Run(60)

	sawStart := false
	for _, s := range probe.Log() {
		if s.StartCondition() {
			sawStart = true
			if len(s.OwnersSDA) != 2 {
				t.Fatalf("START SDA owners = %v, want mA+mB", s.OwnersSDA)
			}
		}
	}
	if !sawStart {
		t.Fatal("probe never observed START condition")
	}

	// 找到败者释放后第一个 tick：OwnersSDA 中不再有 mB。
	released := false
	for _, s := range probe.Log() {
		if !contains(s.OwnersSDA, "mB") && contains(s.OwnersSDA, "mA") && s.SDA == Low {
			released = true
			break
		}
	}
	if !released {
		t.Fatal("loser mB never released SDA while winner continued")
	}
}

func hasEvent(tr []Entry, kind string) bool {
	for _, e := range tr {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
