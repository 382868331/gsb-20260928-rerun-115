package bus

import (
	"fmt"
	"sort"
)

// Recovery 是确定性的总线恢复源：先发传输中复位，再产生最多 9 个 SCL 脉
// 冲，最后在 SCL 高时释放 SDA 形成 STOP。只有真实观测到 STOP 才算成功。
type Recovery struct {
	id      string
	start   int
	running bool
	done    bool
	failed  bool
	group   *RecoveryGroup
}

type recoveryState struct{ running, done, failed bool }

func NewRecovery(id string, startTick int) *Recovery {
	return &Recovery{id: id, start: startTick - startTick%4}
}

func (r *Recovery) ID() string   { return r.id }
func (r *Recovery) Active() bool { return r.running }
func (r *Recovery) Done() bool   { return r.done }
func (r *Recovery) Failed() bool { return r.failed }
func (r *Recovery) EndTick() int { return r.start + 43 }

func (r *Recovery) Drive(tick, phase int) Drive {
	r.activate(tick)
	if !r.running || r.failed || tick < r.start || tick > r.EndTick() {
		return Release
	}
	off := tick - r.start
	switch {
	case off < 4:
		return Drive{SCL: Low, SDA: Low}
	case off <= 39:
		scl := Low
		bitPhase := (off - 4) % 4
		if bitPhase == 1 || bitPhase == 2 {
			scl = High
		}
		return Drive{SCL: scl, SDA: Low}
	default:
		scl, sda := Low, Low
		if phase >= 1 {
			scl = High
		}
		if phase >= 2 {
			sda = High
		}
		return Drive{SCL: scl, SDA: sda}
	}
}

func (r *Recovery) Observe(s Snapshot) {
	r.activate(s.Tick)
	if !r.running || r.failed {
		return
	}
	if s.Tick == r.start+3 && s.Phase == 2 {
		s.Bus.EnqueueEvent(EvReset, r.id, fmt.Sprintf("in-transit reset tick=%d", s.Tick))
		s.Bus.EnqueueEvent(EvRecoverBegin, r.id, fmt.Sprintf("tick=%d", r.start))
	}
	if s.Tick >= r.start+4 && s.Tick <= r.start+39 && s.SCLRising() {
		n := (s.Tick-r.start-4)/4 + 1
		s.Bus.EnqueueEvent(EvRecoverClock, r.id, fmt.Sprintf("pulse=%d", n))
	}
	if s.Tick != r.EndTick() || s.Phase != 2 {
		return
	}
	r.running = false
	if s.StopCondition() {
		r.done = true
		s.Bus.EnqueueEvent(EvRecoverDone, r.id, fmt.Sprintf("tick=%d", s.Tick))
	} else {
		r.failed = true
		s.Bus.EnqueueEvent(EvRecoverFailed, r.id, fmt.Sprintf("tick=%d scl=%s sda=%s", s.Tick, s.SCL, s.SDA))
	}
}

func (r *Recovery) OnEvent(kind, who, detail string) {}

func (r *Recovery) SaveState() interface{} {
	return recoveryState{running: r.running, done: r.done, failed: r.failed}
}

func (r *Recovery) RestoreState(value interface{}) {
	state, _ := value.(recoveryState)
	r.running = state.running
	r.done = state.done
	r.failed = state.failed
}

func (r *Recovery) activate(tick int) {
	if r.running || r.done || r.failed || tick < r.start {
		return
	}
	if r.group != nil && !r.group.allow(r, tick) {
		r.failed = true
		return
	}
	r.running = true
}

// RecoveryGroup 保证多个恢复源同时开始时只有一个能取得总线。
type RecoveryGroup struct {
	id      string
	members []*Recovery
	winner  *Recovery
	b       *Bus
}

func NewRecoveryGroup(id string, recoveries ...*Recovery) *RecoveryGroup {
	g := &RecoveryGroup{id: id}
	for _, recovery := range recoveries {
		g.Add(recovery)
	}
	return g
}

func (g *RecoveryGroup) Add(r *Recovery) {
	r.group = g
	g.members = append(g.members, r)
}

func (g *RecoveryGroup) bind(b *Bus) { g.b = b }

func (g *RecoveryGroup) ID() string                  { return g.id }
func (g *RecoveryGroup) Drive(tick, phase int) Drive { return Release }
func (g *RecoveryGroup) Observe(Snapshot)            {}
func (g *RecoveryGroup) OnEvent(kind, who, detail string) {
}

func (g *RecoveryGroup) allow(r *Recovery, tick int) bool {
	if g.winner == r {
		return true
	}
	if g.winner != nil {
		r.failed = true
		g.b.EnqueueEvent(EvRecoverLost, r.id, fmt.Sprintf("tick=%d", tick))
		return false
	}
	candidates := make([]*Recovery, 0, len(g.members))
	for _, member := range g.members {
		if !member.done && !member.failed && tick >= member.start {
			candidates = append(candidates, member)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].start != candidates[j].start {
			return candidates[i].start < candidates[j].start
		}
		return candidates[i].id < candidates[j].id
	})
	if len(candidates) == 0 {
		return false
	}
	g.winner = candidates[0]
	g.b.EnqueueEvent(EvRecoverGrant, g.winner.id, fmt.Sprintf("tick=%d", tick))
	for _, candidate := range candidates[1:] {
		candidate.failed = true
		if g.b != nil {
			g.b.EnqueueEvent(EvRecoverLost, candidate.id, fmt.Sprintf("tick=%d", tick))
		}
	}
	return g.winner == r
}
