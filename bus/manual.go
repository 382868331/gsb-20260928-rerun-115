package bus

// ManualDevice 是“人工设备”：演练脚本逐 tick 指定它如何拉线，并逐拍读取
// 它看到的电平与边沿。它用于核对每个阶段到底是谁在拉线路、谁在等待。
type ManualDevice struct {
	id string

	driveAt func(tick, phase int) Drive
	log     []Snapshot
}

func NewManualDevice(id string) *ManualDevice {
	return &ManualDevice{
		id:      id,
		driveAt: func(tick, phase int) Drive { return Release },
	}
}

// SetDrive 安装逐拍驱动函数（返回 Release 即不拉线）。
func (d *ManualDevice) SetDrive(fn func(tick, phase int) Drive) {
	if fn == nil {
		fn = func(tick, phase int) Drive { return Release }
	}
	d.driveAt = fn
}

// HoldSDAAbort 模拟一个在收到复位后仍锁住 SDA 的故障设备：
// tick>=fromTick 起一直拉低 SDA，直到 tick>=releaseTick 释放。
func (d *ManualDevice) HoldSDAAbort(fromTick, releaseTick int) {
	d.SetDrive(func(tick, phase int) Drive {
		if tick >= fromTick && tick < releaseTick {
			return Drive{SCL: High, SDA: Low}
		}
		return Release
	})
}

func (d *ManualDevice) ID() string { return d.id }

func (d *ManualDevice) Drive(tick, phase int) Drive {
	return d.driveAt(tick, phase)
}

func (d *ManualDevice) Observe(s Snapshot) {
	d.log = append(d.log, s)
}

// Log 返回该设备观测到的所有快照。
func (d *ManualDevice) Log() []Snapshot { return d.log }

// Last 返回最近一次观测。
func (d *ManualDevice) Last() Snapshot { return d.log[len(d.log)-1] }

func (d *ManualDevice) OnEvent(kind, who, detail string) {}
