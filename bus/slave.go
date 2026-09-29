package bus

// Slave 是教学协议从设备：识别 7 位地址写，逐字节 ACK，采样 SDA 于
// SCL 高电平中点。它从不驱动 SCL（时钟同步留给后续协议扩展）。
type Slave struct {
	id       string
	addr     byte
	selected bool
	bitCnt   int
	shifter  byte
	// 每收到一个字节，在下一个位单元的前三个相位拉低 SDA 作为 ACK，
	// 第四个相位释放，避免在 SCL 上升沿制造假 STOP。
	ackStartTick int
	ackAddrOK    bool
	byteIndex    int
	got          []byte
}

func NewSlave(id string, addr byte) *Slave {
	return &Slave{id: id, addr: addr & 0x7f, ackStartTick: -100}
}

func (s *Slave) ID() string       { return s.id }
func (s *Slave) Bytes() []byte    { return s.got }

func (s *Slave) Drive(tick, phase int) Drive {
	d := Release
	if s.selected && s.ackAddrOK && tick >= s.ackStartTick {
		// ACK 占据一个位单元的相位 0..2，相位 3 释放。
		off := tick - s.ackStartTick
		if off >= 0 && off <= 2 {
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
		s.bitCnt = 0
		s.shifter = 0
		s.byteIndex = 0
		s.ackAddrOK = false
		s.ackStartTick = -100
	case snap.StopCondition():
		s.selected = false
		s.ackAddrOK = false
		s.ackStartTick = -100
	case s.selected && snap.SCLRising():
		s.shifter <<= 1
		if snap.SDA == High {
			s.shifter |= 1
		}
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
	isAddr := s.byteIndex == 0 || s.afterRStart
	_ = isAddr
	// 简化判定：地址字节的最低位是 R/W#；本协议地址出现在每次 START 后，
	// 由 lastWasStart 标记（见下方字段）。
	if s.expectAddr {
		addr := s.shifter >> 1
		rw := s.shifter & 1
		s.ackAddrOK = addr == s.addr && rw == 0
		s.expectAddr = false
		return
	}
	if s.ackAddrOK {
		s.got = append(s.got, s.shifter)
	}
}

func (s *Slave) OnEvent(kind, who, detail string) {
	if kind == EvReset {
		s.selected = false
		s.bitCnt = 0
		s.shifter = 0
		s.ackAddrOK = false
		s.ackStartTick = -100
	}
}
