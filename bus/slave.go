package bus

// SlaveFault 是可预设的从设备故障过程。StretchAt/StretchTicks 描述从指定
// tick 起把 SCL 拉低多久；HoldAt/HoldTicks 描述从指定 tick 起保持 SDA 低电
// 平多久。ClearAt 为故障解除的绝对 tick，取 0 表示该项不启用。
type SlaveFault struct {
	StretchAt    int
	StretchTicks int
	HoldAt       int
	HoldTicks    int
	ClearAt      int
}

// Slave 是教学协议从设备：识别 7 位地址写，逐字节 ACK，并在 SCL 上升沿
// 采样 SDA。故障模式可在指定时间拉长 SCL 或保持 SDA。
type Slave struct {
	id           string
	addr         byte
	selected     bool
	expectAddr   bool
	selectedAddr bool
	bitCnt       int
	edgeNumber   int
	shifter      byte
	ackDrive     bool
	ackPending   bool
	ackArmed     bool
	got          []byte
	fault        SlaveFault
	faultActive  bool
	faultCleared bool
	stretchUntil int
	holdUntil    int
}

func NewSlave(id string, addr byte) *Slave {
	return &Slave{id: id, addr: addr & 0x7f, expectAddr: true}
}

func (s *Slave) ID() string         { return s.id }
func (s *Slave) Bytes() []byte      { return append([]byte(nil), s.got...) }
func (s *Slave) FaultActive() bool  { return s.faultActive }
func (s *Slave) FaultCleared() bool { return s.faultCleared }

func (s *Slave) Drive(tick, phase int) Drive {
	d := Release
	if s.ackDrive {
		d.SDA = Low
	}
	if s.fault.StretchAt > 0 && tick >= s.fault.StretchAt && tick < s.stretchUntil {
		d.SCL = Low
	}
	if s.fault.HoldAt > 0 && tick >= s.fault.HoldAt && tick < s.holdUntil {
		d.SDA = Low
	}
	return d
}

// SetFault 设置可核对的故障脚本；通常应在模拟开始前调用。
func (s *Slave) SetFault(f SlaveFault) {
	s.fault = f
	s.faultActive = false
	s.faultCleared = false
	if f.StretchTicks > 0 && f.StretchAt > 0 {
		s.stretchUntil = f.StretchAt + f.StretchTicks
	} else {
		s.stretchUntil = 0
	}
	if f.HoldTicks > 0 && f.HoldAt > 0 {
		s.holdUntil = f.HoldAt + f.HoldTicks
	} else {
		s.holdUntil = 0
	}
}

// ClearFault 在恢复边界释放故障线路，不改变已移位接收的位。
func (s *Slave) ClearFault() {
	s.fault = SlaveFault{}
	s.faultActive = false
	s.faultCleared = true
	s.stretchUntil = 0
	s.holdUntil = 0
}

func (s *Slave) Observe(snap Snapshot) {
	s.updateFault(snap)
	switch {
	case snap.StartCondition():
		s.selected = true
		s.expectAddr = true
		s.bitCnt = 0
		s.edgeNumber = 0
		s.shifter = 0
		s.selectedAddr = false
		s.ackDrive = false
		s.ackPending = false
		s.ackArmed = false
	case snap.StopCondition():
		s.selected = false
		s.selectedAddr = false
		s.ackDrive = false
		s.ackPending = false
		s.ackArmed = false
	case s.selected && snap.SCLRising():
		if s.ackDrive || s.ackArmed {
			s.edgeNumber++
			return
		}
		bit := byte(0)
		if snap.SDA == High {
			bit = 1
		}
		s.shifter = (s.shifter << 1) | bit
		s.bitCnt++
		s.edgeNumber++
		if s.bitCnt == 8 {
			s.handleByte()
			s.bitCnt = 0
			s.shifter = 0
		}
	case s.selected && snap.SCLFalling():
		if s.ackPending {
			s.ackDrive = true
			s.ackPending = false
			s.ackArmed = true
		} else if s.ackArmed {
			s.ackDrive = false
			s.ackArmed = false
		}
	}
}

func (s *Slave) updateFault(snap Snapshot) {
	f := s.fault
	if f.ClearAt > 0 && snap.Tick >= f.ClearAt && !s.faultCleared {
		s.ClearFault()
		return
	}
	if (f.StretchAt > 0 && snap.Tick >= f.StretchAt && snap.Tick < s.stretchUntil) ||
		(f.HoldAt > 0 && snap.Tick >= f.HoldAt && snap.Tick < s.holdUntil) {
		s.faultActive = true
	}
}

func (s *Slave) handleByte() {
	if s.expectAddr {
		s.selectedAddr = s.shifter == (s.addr<<1)&0xfe
		s.expectAddr = false
		s.ackPending = s.selectedAddr
		return
	}
	if s.selectedAddr {
		s.got = append(s.got, s.shifter)
		s.ackPending = true
	}
}

func (s *Slave) OnEvent(kind, who, detail string) {
	if kind == EvReset {
		s.Reset()
	}
}

// Reset 供恢复设备或测试在两次 Tick 之间清除从设备状态。
func (s *Slave) Reset() {
	id, addr := s.id, s.addr
	*s = Slave{id: id, addr: addr, expectAddr: true}
}

func (s *Slave) SaveState() interface{} { return *s }

func (m *Slave) RestoreState(value interface{}) {
	state, ok := value.(Slave)
	if !ok {
		return
	}
	state.got = append([]byte(nil), state.got...)
	*m = state
}
