// Package bus 定义虚拟总线的电气模型与事件边界。
//
// 教学协议采用类似 I2C 的两线开漏（open-drain）模型：任何设备都只能
// 把 SCL/SDA 拉低（DriveLow）或释放（Release），线电平由线与（wired-AND）
// 决定。设备之间不直接通信，只通过总线上的电平与事件交互，这也是后续
// 扩展其它协议时保持不变的边界。
package bus

import (
	"sort"
	"strings"
)

// Level 是线与之后的实际电平。
type Level int

const (
	High Level = iota // 上拉后的高电平
	Low               // 被一个或多个设备拉低
)

func (l Level) String() string {
	if l == Low {
		return "0"
	}
	return "1"
}

// Drive 是某个设备在一个时刻对两根线的输出意图。
type Drive struct {
	SCL Level // 只能取 Low（拉低）或 High（释放）
	SDA Level
}

// Release 表示设备完全释放总线（高阻）。
var Release = Drive{SCL: High, SDA: High}

// Snapshot 是一次观测时刻总线的完整状态。
type Snapshot struct {
	Tick      int
	Phase     int // 一个位时钟内的四分之一相位 0..3
	SCL       Level
	SDA       Level
	PrevSCL   Level
	PrevSDA   Level
	Drives    map[string]Drive // 当刻所有设备的驱动意图
	OwnersSCL []string         // 当前把 SCL 拉低的设备
	OwnersSDA []string
}

// SCLRising / SCLFalling 等边沿判断，供设备在事件边界上推进状态机。
func (s Snapshot) SCLRising() bool  { return s.PrevSCL == Low && s.SCL == High }
func (s Snapshot) SCLFalling() bool { return s.PrevSCL == High && s.SCL == Low }
func (s Snapshot) SDAFalling() bool { return s.PrevSDA == High && s.SDA == Low }
func (s Snapshot) SDARising() bool  { return s.PrevSDA == Low && s.SDA == High }

// StartCondition 是 SCL 高时 SDA 下降。
func (s Snapshot) StartCondition() bool { return s.SCL == High && s.SDAFalling() }

// StopCondition 是 SCL 高时 SDA 上升。
func (s Snapshot) StopCondition() bool { return s.SCL == High && s.SDARising() }

// 事件种类。事件是协议无关的设备间通知机制。
const (
	EvStart        = "start"     // 观测到 START 条件
	EvStop         = "stop"      // 观测到 STOP 条件，总线回到空闲
	EvArbGrant     = "arb-grant" // 仲裁有了唯一胜者
	EvArbLost      = "arb-lost"  // 某主设备输掉仲裁，必须立刻释放总线
	EvReset        = "reset"     // 中途复位
	EvRecoverBegin = "recovery-begin"
	EvRecoverClock = "recovery-clock"
	EvRecoverDone  = "recovery-done"
	EvRecoverFail  = "recovery-failed"
)

// Entry 是消息记录中的一条：要么记录一段电平，要么记录一个事件。
type Entry struct {
	Tick   int
	Kind   string // "level" 或事件名
	Who    string // 电平段的占有者（拉低者），事件的发起者
	SCL    Level
	SDA    Level
	Detail string
}

// Device 是挂在总线上的一切对象的统一边界：主设备、从设备、仲裁器、
// 恢复脉冲源都只实现这个接口。
type Device interface {
	ID() string
	// Drive 在电平求解阶段被调用，返回本刻驱动意图。
	Drive(tick, phase int) Drive
	// Observe 在电平求解之后被调用，设备只能读不能改。
	Observe(Snapshot)
	// OnEvent 接收协议无关事件（仲裁结果、复位等）。
	OnEvent(kind, who, detail string)
}

// Bus 是两根开漏线与线加上消息记录。
type Bus struct {
	Tick int

	devs    []Device
	pendReg []Device
	pendUn  []Device

	drives           map[string]Drive
	scl, sda         Level
	prevSCL, prevSDA Level

	Trace []Entry
}

func New() *Bus {
	return &Bus{
		drives:  map[string]Drive{},
		scl:     High,
		sda:     High,
		prevSCL: High,
		prevSDA: High,
	}
}

// RunTicks 推进 n 个四分之一相位。
func (b *Bus) RunTicks(n int) {
	for i := 0; i < n; i++ {
		b.Step()
	}
}

// Register 在当前时钟结束后挂载设备（安全的增删时机）。
func (b *Bus) Register(d Device)   { b.pendReg = append(b.pendReg, d) }
func (b *Bus) Unregister(d Device) { b.pendUn = append(b.pendUn, d) }

// ApplyPending 立即处理挂起的挂载/卸载。运行开始前调用可保证所有设备
// 从第一个 tick 起就参与电平求解，不会错过 START 拍。
func (b *Bus) ApplyPending() { b.applyPending() }

func (b *Bus) Devices() []Device { return b.devs }

// Emit 产生一条事件记录并广播。只能在两次 Tick 之间调用。
func (b *Bus) Emit(kind, who, detail string) {
	b.Trace = append(b.Trace, Entry{Tick: b.Tick, Kind: kind, Who: who, Detail: detail})
	for _, d := range b.devs {
		d.OnEvent(kind, who, detail)
	}
}

// Step 推进一个四分之一位时钟：先求驱动，再线与，最后广播观测。
func (b *Bus) Step() {
	phase := b.Tick % 4

	b.drives = make(map[string]Drive, len(b.devs))
	for _, d := range b.devs {
		b.drives[d.ID()] = d.Drive(b.Tick, phase)
	}

	b.prevSCL, b.prevSDA = b.scl, b.sda
	b.scl, b.sda = High, High
	ownerSCL, ownerSDA := map[string]bool{}, map[string]bool{}
	for id, dr := range b.drives {
		if dr.SCL == Low {
			b.scl = Low
			ownerSCL[id] = true
		}
		if dr.SDA == Low {
			b.sda = Low
			ownerSDA[id] = true
		}
	}

	snap := Snapshot{
		Tick: b.Tick, Phase: phase,
		SCL: b.scl, SDA: b.sda,
		PrevSCL: b.prevSCL, PrevSDA: b.prevSDA,
		Drives:    b.drives,
		OwnersSCL: keysOf(ownerSCL), OwnersSDA: keysOf(ownerSDA),
	}

	// 仅在电平变化时记录一段，便于解释“这段电平属于谁”。
	if b.scl != b.prevSCL || b.sda != b.prevSDA {
		who := strings.Join(mergeOwners(snap.OwnersSCL, snap.OwnersSDA), "+")
		if who == "" {
			who = "(pullup)"
		}
		b.Trace = append(b.Trace, Entry{
			Tick: b.Tick, Kind: "level", Who: who,
			SCL: b.scl, SDA: b.sda,
		})
	}

	for _, d := range b.devs {
		d.Observe(snap)
	}

	b.applyPending()
	b.Tick++
}

func (b *Bus) applyPending() {
	for _, d := range b.pendUn {
		for i, x := range b.devs {
			if x.ID() == d.ID() {
				b.devs = append(b.devs[:i], b.devs[i+1:]...)
				break
			}
		}
	}
	b.pendUn = nil
	b.devs = append(b.devs, b.pendReg...)
	b.pendReg = nil
}

// Idle 表示两根线都被释放（高电平）。
func (b *Bus) Idle() bool { return b.scl == High && b.sda == High }

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mergeOwners(a, b []string) []string {
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		m[x] = true
	}
	return keysOf(m)
}
