package bus

import (
	"fmt"
	"sort"
	"strings"
)

// Runner 是总线的“时间过程”：它把所有设备的 Drive/Observe 串到同一条
// Tick 轴上，并承担协议无关的仲裁职责：
//   - 同步发起（同一位单元的相位 0）的主设备进入同一仲裁组；
//   - 输掉逐位仲裁的主设备收到 EvArbLost，必须立即释放并停止驱动；
//   - 仲裁组只剩一个主设备时广播 EvArbGrant，同一时刻至多一个 Granted；
//   - 总线忙时不同步发起的主设备在相位 0 直接判负；
//   - 无响应等待、重复 START、传输中复位都在这同一条 Tick 轴上发生。
type Runner struct {
	b    *Bus
	mats []*Master
	rec  *Recoverer

	last Snapshot

	group       map[string]*Master
	groupStart  int
	tieBroken   bool
	lostEmitted map[string]bool

	prevActive map[string]bool
	prevLost   map[string]bool
}

func NewRunner(b *Bus) *Runner {
	r := &Runner{
		b:           b,
		group:       map[string]*Master{},
		lostEmitted: map[string]bool{},
		prevActive:  map[string]bool{},
		prevLost:    map[string]bool{},
	}
	r.rec = NewRecoverer(b)
	b.Register(r)
	b.Register(r.rec)
	b.ApplyPending()
	return r
}

func (r *Runner) ID() string { return "runner" }
func (r *Runner) Drive(tick, phase int) Drive {
	// 仲裁器/调度器只看线，不驱动线。
	return Release
}

func (r *Runner) Observe(s Snapshot) { r.last = s }
func (r *Runner) OnEvent(kind, who, detail string) {
	// 事件由 Runner 自己产生，这里无需处理。
}

// Master 登记一个主设备并同时挂到总线上。
func (r *Runner) Master(m *Master) {
	r.mats = append(r.mats, m)
	r.b.Register(m)
	r.b.ApplyPending()
}

// Attach 把任意额外设备（从设备、人工设备）挂到总线上。
func (r *Runner) Attach(d Device) {
	r.b.Register(d)
	r.b.ApplyPending()
}

// Step 推进一个四分之一相位，并在电平求解之后处理仲裁与恢复事件。
func (r *Runner) Step() {
	r.b.Step()
	r.postTick()
}

// Run 推进 n 个四分之一相位。
func (r *Runner) Run(n int) {
	for i := 0; i < n; i++ {
		r.Step()
	}
}

// AbortReset 在两次 Tick 之间广播一次传输中复位；所有主设备立即中止事务。
func (r *Runner) AbortReset(who string) {
	r.b.Emit(EvReset, who, "in-transit reset")
}

// Recover 在两次 Tick 之间发起复位 + 9 个恢复时钟 + STOP 的完整恢复过程，
// 从下一个相位开始执行。返回完成时刻的 tick。
func (r *Runner) Recover(who string) int {
	r.b.Emit(EvReset, who, "reset before recovery")
	r.b.Emit(EvRecoverBegin, who, "9 clock pulses + STOP")
	r.rec.Begin(r.b.Tick + 1)
	return r.b.Tick + r.rec.LenPhases()
}

func (r *Runner) postTick() {
	s := r.last

	// 1) 事件化的 START/STOP 只用于记录“谁在拉线路”。
	if s.StartCondition() {
		r.b.Emit(EvStart, strings.Join(s.OwnersSDA, "+"), "")
	}
	if s.StopCondition() {
		r.b.Emit(EvStop, strings.Join(s.OwnersSDA, "+"), "bus idle")
		r.resetGroup()
	}

	// 2) 收集本刻新发起与刚输掉仲裁的主设备。
	var joined []*Master
	for _, m := range r.mats {
		id := m.id
		if m.active && !r.prevActive[id] && !m.lost {
			joined = append(joined, m)
		}
		if m.lost && !r.prevLost[id] {
			if !r.lostEmitted[id] {
				r.lostEmitted[id] = true
				r.b.Emit(EvArbLost, id, "bit-level arbitration")
			}
			delete(r.group, id)
		}
	}

	// 3) 新发起者入仲裁组。总线忙时不同步发起者直接判负。
	for _, m := range joined {
		if len(r.group) > 0 && !(s.SCL == High && s.SDA == High) {
			m.loseLocal(errArb(m.id, "start while bus busy"))
			r.lostEmitted[m.id] = true
			r.b.Emit(EvArbLost, m.id, "unsynchronized start while bus busy")
			continue
		}
		if len(r.group) == 0 {
			r.groupStart = s.Tick - s.Phase
			r.tieBroken = false
		}
		r.group[m.id] = m
	}

	// 4) 仲裁组收敛到唯一胜者时授予总线。
	r.grantIfUnique()

	// 5) 位流完全相同的多主设备：在地址字节应答位结束时按固定优先级破平。
	if !r.tieBroken && len(r.group) > 1 && s.Phase == 3 {
		cell := (s.Tick - s.Phase - r.groupStart) / 4
		if cell == 9 { // 单元 0=START,1..8=地址位,9=地址 ACK
			r.tieBroken = true
			ids := make([]string, 0, len(r.group))
			for id := range r.group {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			winner := ids[0]
			for _, id := range ids[1:] {
				lm := r.group[id]
				lm.loseLocal(errArb(id, "tie broken by fixed priority after address"))
				r.lostEmitted[id] = true
				r.b.Emit(EvArbLost, id, "identical bitstream, fixed priority")
				delete(r.group, id)
			}
			_ = winner
			r.grantIfUnique()
		}
	}

	// 6) 恢复过程的时钟与完成事件。
	if r.rec.Active() {
		r.rec.ObserveTick(r.b, s)
	}

	// 7) 主设备事务完成提交。
	for _, m := range r.mats {
		m.TickCommit(s.Tick)
		r.prevActive[m.id] = m.active
		r.prevLost[m.id] = m.lost
	}
}

func (r *Runner) grantIfUnique() {
	if len(r.group) != 1 {
		return
	}
	for id, m := range r.group {
		if !m.granted {
			m.granted = true
			r.b.Emit(EvArbGrant, id, "unique owner of the bus")
		}
	}
}

func (r *Runner) resetGroup() {
	r.group = map[string]*Master{}
	r.lostEmitted = map[string]bool{}
	r.tieBroken = false
	// STOP 后所有主设备都不再 active；清掉边沿记忆，使下一次发起
	// 被正确识别为新的同步发起，而不是“总线忙时中途加入”。
	r.prevActive = map[string]bool{}
	r.prevLost = map[string]bool{}
}

func errArb(id, detail string) error {
	return fmt.Errorf("%s: %w (%s)", id, ErrArbLost, detail)
}
