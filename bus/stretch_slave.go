package bus

// StretchSlave 在普通从设备基础上增加可控的“暂停-接续-退出”脚本：
//   - PauseAfterByte(n, k)：应答第 n 个数据字节（全组计数）后拉低 SCL
//     暂停 k 拍（时钟拉伸），随后释放，传输接续；
//   - QuitAfterBytes(m)：收到第 m 个数据字节后停止应答（退出），主设备
//     在下一字节得到 NACK 并以 STOP 结束整组；
//   - SetReadData：支持读操作，按预置字节序列向主设备发送数据。
//
// 位采样跟随 SCL 上升沿、读数据位跟随 SCL 下降沿推进，因此主设备被
// 拉伸顺延任意拍数后，从设备仍与主设备的实际时钟对齐。
type StretchSlave struct {
	id           string
	addr         byte
	selected     bool
	expectAddr   bool
	selectedAddr bool
	selectedRead bool
	readDone     bool
	bitCnt       int
	shifter      byte
	byteIndex    int

	ackStartTick  int
	firstDataTick int
	got           []byte
	dataSeen      int

	pauseAfter   map[int]int
	quitAt       int
	stretchFrom  int
	stretchUntil int

	readData    []byte
	readUnit    int
	readArmTick int
}

func NewStretchSlave(id string, addr byte) *StretchSlave {
	return &StretchSlave{
		id: id, addr: addr & 0x7f, expectAddr: true,
		ackStartTick: -100, firstDataTick: -100,
		stretchFrom: -100, stretchUntil: -100,
		pauseAfter: map[int]int{}, quitAt: -1, readUnit: -1, readArmTick: -100,
	}
}

func (s *StretchSlave) ID() string    { return s.id }
func (s *StretchSlave) Bytes() []byte { return s.got }

// SetReadData 设置读操作时发给主设备的字节序列。
func (s *StretchSlave) SetReadData(data []byte) { s.readData = append([]byte(nil), data...) }

// PauseAfterByte 在第 n 个数据字节（0 起，全组计数）的 ACK 之后拉伸 SCL k 拍。
func (s *StretchSlave) PauseAfterByte(n, ticks int) { s.pauseAfter[n] = ticks }

// QuitAfterBytes 在应答过 m 个数据字节后停止应答（退出传输）。
func (s *StretchSlave) QuitAfterBytes(m int) { s.quitAt = m }

func (s *StretchSlave) Drive(tick, phase int) Drive {
	d := Release
	if tick >= s.stretchFrom && tick < s.stretchUntil {
		d.SCL = Low
	}
	if s.selected && s.selectedAddr && tick >= s.ackStartTick {
		if off := tick - s.ackStartTick; off >= 0 && off <= 3 {
			d.SDA = Low
		}
	}
	if s.selected && s.selectedAddr && s.selectedRead && !s.readDone && s.readUnit >= 0 {
		if s.readUnit%9 != 8 { // 每 9 个位单元的第 9 个是主设备应答位
			byteIdx := s.readUnit / 9
			if byteIdx < len(s.readData) {
				bit := 7 - s.readUnit%9
				if s.readData[byteIdx]&(1<<uint(bit)) == 0 {
					d.SDA = Low
				}
			}
		}
	}
	return d
}

func (s *StretchSlave) Observe(snap Snapshot) {
	switch {
	case snap.StartCondition():
		s.selected = true
		s.expectAddr = true
		s.bitCnt = 0
		s.shifter = 0
		s.byteIndex = 0
		s.selectedAddr = false
		s.selectedRead = false
		s.readDone = false
		s.readUnit = -1
		s.ackStartTick = -100
		s.firstDataTick = snap.Tick - snap.Phase + 4
	case snap.StopCondition():
		s.selected = false
		s.selectedAddr = false
		s.selectedRead = false
		s.ackStartTick = -100
		s.dataSeen = 0
	case s.selected && s.selectedRead && !s.readDone && snap.SCLFalling() && snap.Tick >= s.readArmTick:
		// 读操作：每个 SCL 下降沿进入下一个读位单元。
		s.readUnit++
	case s.selected && s.selectedRead && !s.readDone && snap.SCLRising() && s.readUnit >= 0 && s.readUnit%9 == 8:
		// 读操作：主设备应答位上观测到 NACK 表示读结束。
		if snap.SDA == High {
			s.readDone = true
		}
	case s.selected && !s.selectedRead && snap.Tick >= s.firstDataTick && snap.SCLRising():
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

func (s *StretchSlave) handleByte(snap Snapshot) {
	// ACK 占据下一个位单元；以观测到的上升沿为基准，与拉伸顺延兼容。
	s.ackStartTick = snap.Tick + 3
	s.firstDataTick = s.ackStartTick + 4
	if s.expectAddr {
		s.selectedAddr = s.shifter>>1 == s.addr
		s.selectedRead = s.shifter&1 == 1
		s.expectAddr = false
		if !s.selectedAddr {
			s.ackStartTick = -100
			return
		}
		if s.selectedRead {
			s.readUnit = -1
			s.readArmTick = s.ackStartTick + 3
		}
		return
	}
	if !s.selectedAddr {
		return
	}
	if s.quitAt >= 0 && s.dataSeen >= s.quitAt {
		// 退出：不应答，也不再响应后续字节。
		s.selectedAddr = false
		s.ackStartTick = -100
		return
	}
	s.got = append(s.got, s.shifter)
	if p, ok := s.pauseAfter[s.dataSeen]; ok && p > 0 {
		s.stretchFrom = s.ackStartTick + 4
		s.stretchUntil = s.stretchFrom + p
	}
	s.dataSeen++
}

func (s *StretchSlave) OnEvent(kind, who, detail string) {
	if kind == EvReset {
		s.Reset()
	}
}

// Reset 供恢复设备或测试在两次 Tick 之间清除从设备状态。
func (s *StretchSlave) Reset() {
	s.selected = false
	s.expectAddr = true
	s.bitCnt = 0
	s.shifter = 0
	s.byteIndex = 0
	s.selectedAddr = false
	s.selectedRead = false
	s.readDone = false
	s.ackStartTick = -100
	s.firstDataTick = -100
	s.stretchFrom = -100
	s.stretchUntil = -100
	s.dataSeen = 0
	s.readUnit = -1
	s.readArmTick = -100
}
