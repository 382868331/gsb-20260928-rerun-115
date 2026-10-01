package bus

// Runner 将电平求解、设备观察、事件广播和主设备提交放进同一个时间过程。
// Bus.Observe 内部产生的仲裁事件延迟到当前快照观察阶段结束后处理，保证输
// 掉仲裁的设备从下一相位起释放，而本相位的电气结果仍可复现。
type Runner struct {
	Bus     *Bus
	masters []*Master
}

func NewRunner(b *Bus, devs ...Device) *Runner {
	r := &Runner{Bus: b}
	for _, dev := range devs {
		if master, ok := dev.(*Master); ok {
			r.masters = append(r.masters, master)
		}
		b.Register(dev)
	}
	for _, dev := range b.pendReg {
		if group, ok := dev.(*RecoveryGroup); ok {
			group.bind(b)
		}
	}
	return r
}

func (r *Runner) Step() {
	b := r.Bus
	b.Tick()
	for _, master := range r.masters {
		master.TickCommit(b.Now)
	}
}

func (r *Runner) Run(ticks int) {
	for i := 0; i < ticks; i++ {
		r.Step()
	}
}
