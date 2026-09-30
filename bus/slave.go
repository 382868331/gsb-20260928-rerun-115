package bus

// Slave 是教学协议从设备：识别 7 位地址写，逐字节 ACK，采样 SDA 于
// SCL 高电平中点（即主设备相位 2）。它从不驱动 SCL（时钟同步/拉伸不在
// 本教学协议支持范围内，见 README）。
type Slave struct {
	id   string
	addr byte

	selected bool // 自最近一次 START 起正在接收
	addrNext bool // 下一个收满的 8 位是地址字节

	bitCnt  int
	shifter byte

	// ACK 脉冲仅占据紧随接收字节之后的一个位单元的相位 0..2。
	ackCellStart int // 该位单元的相位 0 tick；<0 表示没有 ACK 待发
	skipAckCell  int // 紧随其后的 ACK 位单元起点，期间上升沿不采样数据
	ackAddrOK    bool
	got          []byte
}

func NewSlave(id string, addr byte) *Slave {
	return &Slave{id: id, addr: addr & 0x7f, ackCellStart: -1, skipAckCell: -1}
}

func (s *Slave) ID() string    { return s.id }
func (s *Slave) Bytes() []byte { return s.got }

func (s *Slave) Drive(tick, phase int) Drive {
	d := Release
	if !s.ackAddrOK || s.ackCellStart < 0 {
		return d
	}
	// 仅在当前 ACK 位单元的相位 0..2 拉低 SDA；相位 3 及之后必须释放，
	// 否则会把主设备后续发送的高电平数据位强行拉低，制造假仲裁失败。
	if tick >= s.ackCellStart && tick < s.ackCellStart+3 {
		d.SDA = Low
	}
	return d
}

func (s *Slave) Observe(snap Snapshot) {
	switch {
	case snap.StartCondition():
		// START 与重复 START 都让从设备重新按地址匹配。
		s.selected = true
		s.addrNext = true
		s.bitCnt = 0
		s.shifter = 0
		s.ackAddrOK = false
		s.ackCellStart = -1
		s.skipAckCell = -1
	case snap.StopCondition():
		s.selected = false
		s.addrNext = false
		s.ackAddrOK = false
		s.ackCellStart = -1
		s.skipAckCell = -1
	case s.selected && snap.SCLRising():
		// ACK 位单元由从设备自己驱动 SDA，其中的 SCL 上升沿不是数据位。
		if s.skipAckCell >= 0 && snap.Tick-snap.Phase == s.skipAckCell {
			return
		}
		s.shifter <<= 1
		if snap.SDA == High {
			s.shifter |= 1
		}
		s.bitCnt++
		if s.bitCnt == 8 {
			s.handleByte(snap)
			s.bitCnt = 0
			s.shifter = 0
		}
	default:
		// ACK 位单元结束（相位 3）后立刻撤掉待发标记。
		if s.ackCellStart >= 0 && snap.Tick == s.ackCellStart+3 {
			s.ackCellStart = -1
		}
	}
}

func (s *Slave) handleByte(snap Snapshot) {
	// ACK 位从下一个位单元的相位 0（当前上升沿所在单元之后）开始。
	s.ackCellStart = snap.Tick - snap.Phase + 4
	s.skipAckCell = s.ackCellStart
	if s.addrNext {
		addr := s.shifter >> 1
		rw := s.shifter & 1
		s.ackAddrOK = addr == s.addr && rw == 0
		s.addrNext = false
		return
	}
	if s.ackAddrOK {
		s.got = append(s.got, s.shifter)
	}
}

func (s *Slave) OnEvent(kind, who, detail string) {
	if kind == EvReset || kind == EvRecoverBegin {
		s.selected = false
		s.addrNext = false
		s.bitCnt = 0
		s.shifter = 0
		s.ackAddrOK = false
		s.ackCellStart = -1
		s.skipAckCell = -1
	}
}
