package bus

// Runner 将电平求解、设备观察、事件广播和主设备提交放进同一个时间过程。
// Bus.Observe 内部产生的仲裁事件延迟到当前快照观察阶段结束后处理，保证输
// 掉仲裁的设备从下一相位起释放，而本相位的电气结果仍可复现。
type Runner struct {
	Bus     *Bus
	masters []*Master
	devs    []Device
}

func NewRunner(b *Bus, devs ...Device) *Runner {
	r := &Runner{Bus: b}
	for _, dev := range devs {
		if master, ok := dev.(*Master); ok {
			r.masters = append(r.masters, master)
		}
		r.devs = append(r.devs, dev)
		b.Register(dev)
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

// Checkpoint 是组内任意一位上的保存点：记录当前拍、波形长度、各主机已
// 确认操作数与各从设备已收字节数。保存是只读的，之后可继续推进并核对。
type Checkpoint struct {
	Tick       int
	TraceLen   int
	Confirmed  map[string]int
	SlaveBytes map[string]int
}

// Checkpoint 在组内任意一位上保存进度快照，随后可继续运行。
func (r *Runner) Checkpoint() Checkpoint {
	cp := Checkpoint{
		Tick:       r.Bus.Now,
		TraceLen:   len(r.Bus.Trace),
		Confirmed:  map[string]int{},
		SlaveBytes: map[string]int{},
	}
	for _, master := range r.masters {
		cp.Confirmed[master.ID()] = master.ConfirmedOps()
	}
	for _, dev := range r.devs {
		if slave, ok := dev.(*Slave); ok {
			cp.SlaveBytes[slave.ID()] = len(slave.Bytes())
		}
	}
	return cp
}
