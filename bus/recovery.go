package bus

import "strings"

// Recoverer 是总线恢复脉冲源：当某设备把线路锁死（典型为从设备卡在
// ACK 位持续拉低 SDA）时，按 I2C 类协议的标准手法产生至多 9 个 SCL
// 时钟脉冲，直到 SDA 释放，再补一个 STOP，让两根线都回到高电平。
//
// 它与主设备处于同一条 Tick 时间轴：复位事件 EvReset 先让所有主/从设备
// 停止驱动，然后 Recoverer 从指定 tick 开始接管 SCL。
type Recoverer struct {
	b *Bus

	beginTick int // <=0 表示未在恢复
	endTick   int
	pulses    int
	done      bool
}

func NewRecoverer(b *Bus) *Recoverer {
	return &Recoverer{b: b, beginTick: -1}
}

func (r *Recoverer) ID() string { return "recoverer" }

// Begin 在 beginTick 开始恢复；beginTick 必须对齐相位 0。
func (r *Recoverer) Begin(beginTick int) {
	r.beginTick = beginTick
	r.endTick = beginTick + 10*4 // 9 个脉冲 + 1 个 STOP 位单元
	r.pulses = 0
	r.done = false
}

func (r *Recoverer) Active() bool { return r.beginTick > 0 && !r.done }

// LenPhases 返回一次恢复占用的四分之一相位总数（40）。
func (r *Recoverer) LenPhases() int { return 10 * 4 }

func (r *Recoverer) Drive(tick, phase int) Drive {
	if !r.Active() || tick < r.beginTick || tick >= r.endTick {
		return Release
	}
	off := tick - r.beginTick
	cell := off / 4
	ph := off % 4
	if cell < 9 {
		// 恢复脉冲：相位 0 保持 SCL 低给从设备释放 SDA 的窗口，
		// 相位 1..2 抬高 SCL，相位 3 拉低。SDA 由锁死的设备控制，
		// Recoverer 不碰 SDA。
		d := Release
		if ph == 0 || ph == 3 {
			d.SCL = Low
		}
		return d
	}
	// STOP 位单元：若 SDA 已为高，释放即可（上拉形成 STOP 后空闲）；
	// 若 SDA 仍被外部锁死，Recoverer 无法强行制造 STOP，保持释放。
	return Release
}

// ObserveTick 在每个 tick 后由 Runner 调用，按线与结果推进恢复状态机。
func (r *Recoverer) ObserveTick(b *Bus, s Snapshot) {
	if !r.Active() || s.Tick < r.beginTick {
		return
	}
	off := s.Tick - r.beginTick
	cell := off / 4
	ph := off % 4

	// 每个脉冲的 SCL 高电平中点计数并记录一次。
	if cell < 9 && ph == 2 {
		r.pulses = cell + 1
		b.Emit(EvRecoverClock, r.ID(), "")
	}

	if cell == 9 {
		// STOP 位单元结束：SDA 高（被释放）才算恢复成功。
		if ph == 3 {
			r.done = true
			if s.SDA == High && s.SCL == High {
				b.Emit(EvRecoverDone, r.ID(), "SDA released, STOP issued")
			} else {
				holders := strings.Join(mergeOwners(s.OwnersSCL, s.OwnersSDA), "+")
				b.Emit(EvRecoverFail, r.ID(), "line still held by "+holders)
			}
			r.beginTick = -1
		}
	}
}

func (r *Recoverer) Observe(s Snapshot) {}

func (r *Recoverer) OnEvent(kind, who, detail string) {}
