package bus

// Slave 是教学协议从设备：识别 7 位地址写，逐字节 ACK，采样 SDA 于
// SCL 高电平中点。它从不驱动 SCL（时钟同步留给后续协议扩展）。
type Slave struct {
	id         string
	addr       byte
	selected   bool
	expectAddr bool
	bitCnt     int
	shifter    byte
	// 每收到一个字节，在应答位单元拉低 SDA；下一位单元开始时释放。
	ackStartTick  int
	selectedAddr  bool
	byteIndex     int
	got           []byte
	firstDataTick int
}

func NewSlave(id string, addr byte) *Slave {
	return &Slave{id: id, addr: addr & 0x7f, expectAddr: true, ackStartTick: -100, firstDataTick: -100}
}

func (s *Slave) ID() string    { return s.id }
func (s *Slave) Bytes() []byte { return s.got }

func (s *Slave) Drive(tick, phase int) Drive {
	d := Release
	if s.selected && s.selectedAddr && tick >= s.ackStartTick {
		// ACK 占据整个应答位单元，下一位单元开始时释放。
		off := tick - s.ackStartTick
		if off >= 0 && off <= 3 {
			d.SDA = Low
		}
	}
	return d
}

func (s *Slave) Observe(snap Snapshot) {
	switch {
	case snap.StartCondition():
		// START 与重复 START 都让从设备重新按地址匹配。
		s.selected = true
		s.expectAddr = true
		s.bitCnt = 0
		s.shifter = 0
		s.byteIndex = 0
		s.selectedAddr = false
		s.ackStartTick = -100
		s.firstDataTick = snap.Tick - snap.Phase + 4
	case snap.StopCondition():
		s.selected = false
		s.selectedAddr = false
		s.ackStartTick = -100
	case s.selected && snap.Tick >= s.firstDataTick && snap.SCL == High && snap.Phase == 1:
		bit := byte(0)
		if snap.SDA == High {
			bit = 1
		}
		s.shifter = (s.shifter << 1) | bit
		s.bitCnt++
		if s.bitCnt == 8 {
			s.handleByte(snap)
			s.bitCnt = 0
			s.shifter = 0
			s.byteIndex++
		}
	}
}

func (s *Slave) handleByte(snap Snapshot) {
	// ACK 位从下一个相位 0（即当前上升沿之后）开始。
	s.ackStartTick = snap.Tick - snap.Phase + 4
	s.firstDataTick = s.ackStartTick + 4
	if s.expectAddr {
		s.selectedAddr = s.shifter == (s.addr<<1)&0xfe
		s.expectAddr = false
		return
	}
	if s.selectedAddr {
		s.got = append(s.got, s.shifter)
	}
}

func (s *Slave) OnEvent(kind, who, detail string) {
	if kind == EvReset {
		s.selected = false
		s.bitCnt = 0
		s.shifter = 0
		s.selectedAddr = false
		s.ackStartTick = -100
	}
}

// Reset 供恢复设备或测试在两次 Tick 之间清除从设备状态。
func (s *Slave) Reset() {
	s.selected = false
	s.expectAddr = true
	s.bitCnt = 0
	s.shifter = 0
	s.byteIndex = 0
	s.selectedAddr = false
	s.ackStartTick = -100
	s.firstDataTick = -100
}
