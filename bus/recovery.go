package bus

import "fmt"

// Recovery 是确定性的总线恢复源：先发传输中复位，再产生最多 9 个 SCL 脉
// 冲，最后在 SCL 高时释放 SDA 形成 STOP。恢复源在恢复期间拥有 SCL；其
// 它主设备收到 reset 后必须停止驱动并保留本次失败记录。
type Recovery struct {
	id      string
	start   int
	running bool
	done    bool
}

func NewRecovery(id string, startTick int) *Recovery {
	return &Recovery{id: id, start: startTick - startTick%4}
}

func (r *Recovery) ID() string   { return r.id }
func (r *Recovery) Active() bool { return r.running }
func (r *Recovery) Done() bool   { return r.done }
func (r *Recovery) EndTick() int { return r.start + 43 }

func (r *Recovery) Drive(tick, phase int) Drive {
	r.activate(tick)
	if !r.running || tick < r.start || tick > r.EndTick() {
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
	if !r.running {
		return
	}
	if s.Tick == r.start+3 && s.Phase == 2 {
		s.Bus.EnqueueEvent(EvReset, r.id, "in-transit reset")
		s.Bus.EnqueueEvent(EvRecoverBegin, r.id, fmt.Sprintf("tick=%d", r.start))
	}
	if s.Tick >= r.start+4 && s.Tick <= r.start+39 && s.SCLRising() {
		n := (s.Tick-r.start-4)/4 + 1
		s.Bus.EnqueueEvent(EvRecoverClock, r.id, fmt.Sprintf("pulse=%d", n))
	}
	if s.Tick == r.EndTick() && s.Phase == 2 && s.StopCondition() {
		r.running = false
		r.done = true
		s.Bus.EnqueueEvent(EvRecoverDone, r.id, fmt.Sprintf("tick=%d", s.Tick))
	}
}

func (r *Recovery) OnEvent(kind, who, detail string) {}

func (r *Recovery) activate(tick int) {
	if !r.running && !r.done && tick >= r.start {
		r.running = true
	}
}
