package bus

// committer 是每拍结束后需要提交检查的设备（Master、GroupMaster）。
type committer interface {
	TickCommit(tick int)
}

// Runner 将电平求解、设备观察、事件广播和主设备提交放进同一个时间过程。
// Bus.Observe 内部产生的仲裁事件延迟到当前快照观察阶段结束后处理，保证输
// 掉仲裁的设备从下一相位起释放，而本相位的电气结果仍可复现。
type Runner struct {
	Bus        *Bus
	committers []committer
}

func NewRunner(b *Bus, devs ...Device) *Runner {
	r := &Runner{Bus: b}
	r.Add(devs...)
	return r
}

// Add 在运行中途挂载更多设备（例如稍后创建的竞争主设备）。
func (r *Runner) Add(devs ...Device) {
	for _, dev := range devs {
		if c, ok := dev.(committer); ok {
			r.committers = append(r.committers, c)
		}
		r.Bus.Register(dev)
	}
}

func (r *Runner) Step() {
	b := r.Bus
	b.Tick()
	for _, c := range r.committers {
		c.TickCommit(b.Now)
	}
}

func (r *Runner) Run(ticks int) {
	for i := 0; i < ticks; i++ {
		r.Step()
	}
}
