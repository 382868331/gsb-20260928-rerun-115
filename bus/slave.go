package bus

// SlaveScript 描述从设备在组中途的可控行为：提供读数据、在某个读位上
// 拉低 SCL 暂停若干位周期后接续，以及在第 N 次被寻址时不再应答并退出。
type SlaveScript struct {
	ReadData    []byte // 读操作依次提供的字节
	StretchBit  int    // 要暂停的读位窗口序号；-1 表示不暂停
	StretchBits int    // 暂停持续的位周期数
	QuitAtAddr  int    // 第几次（0 起）被寻址时不应答并退出；-1 表示不退出
}

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

	script       SlaveScript
	hasScript    bool
	reading      bool // 当前处于读操作，正在输出读数据
	readBase     int  // 读位窗口 0 的起始拍
	schedDelay   int  // 自身拉伸造成的后续窗口偏移
	stretched    bool
	stretchUntil int
	addrSeen     int
	detached     bool
}

func NewSlave(id string, addr byte) *Slave {
	return &Slave{id: id, addr: addr & 0x7f, expectAddr: true, ackStartTick: -100, firstDataTick: -100}
}

// SetScript 挂载可控行为脚本。
func (s *Slave) SetScript(script SlaveScript) {
	s.script = script
	s.hasScript = true
}

func (s *Slave) ID() string     { return s.id }
func (s *Slave) Bytes() []byte  { return s.got }
func (s *Slave) Detached() bool { return s.detached }

func (s *Slave) Drive(tick, phase int) Drive {
	d := Release
	if s.detached {
		return d
	}
	if s.selected && s.selectedAddr && tick >= s.ackStartTick {
		// ACK 占据整个应答位单元，下一位单元开始时释放。
		off := tick - s.ackStartTick
		if off >= 0 && off <= 3 {
			d.SDA = Low
		}
	}
	if s.reading {
		if s.hasScript && s.script.StretchBit >= 0 && !s.stretched {
			if w := tick - s.readBase - s.schedDelay; w >= 0 && w%4 == 0 && w/4 == s.script.StretchBit {
				// 暂停：从本位窗口起拉低 SCL，持续指定位周期。
				s.stretched = true
				s.stretchUntil = tick + 4*s.script.StretchBits
				s.schedDelay += 4 * s.script.StretchBits
			}
		}
		if s.stretched && tick < s.stretchUntil {
			d.SCL = Low
			if s.readBitValue(s.script.StretchBit) == 0 {
				d.SDA = Low
			}
			return d
		}
		if w := tick - s.readBase - s.schedDelay; w >= 0 {
			if idx := w / 4; idx%9 != 8 && s.readBitValue(idx) == 0 {
				d.SDA = Low
			}
		}
	}
	return d
}

// readBitValue 返回读位窗口 idx（含每第 9 个主机应答窗口）对应的数据位。
func (s *Slave) readBitValue(idx int) int {
	j := (idx/9)*8 + idx%9
	if j/8 >= len(s.script.ReadData) {
		return 1
	}
	return int((s.script.ReadData[j/8] >> uint(7-j%8)) & 1)
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
		s.reading = false
		s.schedDelay = 0
		s.stretched = false
		s.stretchUntil = 0
	case snap.StopCondition():
		s.selected = false
		s.selectedAddr = false
		s.ackStartTick = -100
		s.reading = false
	case s.reading && snap.Tick >= s.readBase+s.schedDelay && snap.Phase == 1:
		// 读操作的主机应答窗口：主机回 NACK（高）表示读操作结束。
		if w := snap.Tick - s.readBase - s.schedDelay; w%4 == 1 && (w/4)%9 == 8 && snap.SDA == High {
			s.reading = false
		}
	case s.selected && !s.reading && snap.Tick >= s.firstDataTick && snap.SCL == High && snap.Phase == 1:
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
		s.expectAddr = false
		matched := s.shifter>>1 == s.addr
		if s.hasScript && s.script.QuitAtAddr >= 0 && s.addrSeen == s.script.QuitAtAddr {
			// 最终退出：不应答本次寻址，并从总线上摘除自己。
			s.addrSeen++
			s.selectedAddr = false
			s.detached = true
			snap.Bus.Unregister(s)
			return
		}
		s.addrSeen++
		s.selectedAddr = matched
		if matched && s.shifter&1 == 1 {
			// 读地址命中：地址 ACK 之后开始输出读数据。
			s.reading = true
			s.readBase = s.firstDataTick
		}
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
	s.reading = false
	s.schedDelay = 0
	s.stretched = false
	s.stretchUntil = 0
}
