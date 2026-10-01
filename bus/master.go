package bus

import "fmt"

// Frame 是一次寻址后的一段写传输（教学协议：7 位地址 + 若干写字节）。
type Frame struct {
	Addr byte // 7 位从地址
	Data []byte
}

// TxResult 是一次事务的结果；从设备无响应等情况以错误返回而不是永久等待。
type TxResult struct {
	Frames []Frame
	Err    error
}

// Master 是主设备。它把事务预先编排成若干“位段”，每个位段固定占 4 个
// 四分之一时钟（SCL 的低/上升/高/下降相位）。输掉仲裁时立即丢弃编排、
// 释放总线，绝不继续驱动。
type Master struct {
	id        string
	b         *Bus
	start     int
	segs      []seg
	active    bool
	lost      bool
	done      bool
	result    TxResult
	doneCh    chan TxResult
	attempts  []TxResult
	lostAt    int
	busFreeAt int
	stall     int
	waitSince int
	timeout   int
	blocked   bool
}

type segKind int

const (
	segStart segKind = iota
	segRStart
	segBit // SDA 在整个位单元保持 val
	segAck // 主设备释放 SDA，读从设备应答
	segStop
)

type seg struct {
	kind  segKind
	val   Level
	label string
}

func NewMaster(id string, b *Bus, startTick int) *Master {
	return &Master{id: id, b: b, start: startTick, doneCh: make(chan TxResult, 16), busFreeAt: -1}
}

// SetClockStretchTimeout 设置主设备等待 SCL 变为高电平的最大 tick 数；0 表
// 示无限等待。超时只中止本次事务并留下事件，不会擅自宣布恢复成功。
func (m *Master) SetClockStretchTimeout(ticks int) {
	if ticks >= 0 {
		m.timeout = ticks
	}
}

func (m *Master) ID() string { return m.id }

// Done 在事务结束（成功、NACK、仲裁失败均算结束）时可读。
func (m *Master) Done() <-chan TxResult  { return m.doneCh }
func (m *Master) Result() TxResult       { return m.result }
func (m *Master) Active() bool           { return m.active }
func (m *Master) Lost() bool             { return m.lost }
func (m *Master) LostAt() int            { return m.lostAt }
func (m *Master) Blocked() bool          { return m.blocked }
func (m *Master) PendingFrames() []Frame { return append([]Frame(nil), m.result.Frames...) }
func (m *Master) Attempts() []TxResult   { return append([]TxResult(nil), m.attempts...) }

func (m *Master) ArbitratingAt(tick, phase int) bool {
	_, current, ok := m.segmentAt(tick)
	return ok && current.kind == segBit && (m.logicalTick(tick)-1-m.start)%4 == 1
}

func (m *Master) DataBitAt(tick, phase int) bool {
	_, current, ok := m.segmentAt(tick)
	return ok && current.kind == segBit
}

func (m *Master) logicalTick(tick int) int { return tick - m.stall }

func (m *Master) segmentAt(tick int) (int, seg, bool) {
	logical := m.logicalTick(tick)
	if !m.active || m.lost || logical <= m.start {
		return 0, seg{}, false
	}
	idx := (logical - 1 - m.start) / 4
	if idx < 0 || idx >= len(m.segs) {
		return idx, seg{}, false
	}
	return idx, m.segs[idx], true
}

func (m *Master) ParticipatingAt(tick int) bool {
	return m.active && !m.lost && !m.blocked && tick > m.start+m.stall
}

// CanRetryAt 报告输掉仲裁的设备在指定时刻是否已等到一个完整空闲位周期。
func (m *Master) CanRetryAt(tick int) bool {
	return m.lost && m.busFreeAt >= 0 && tick >= m.busFreeAt
}

// EarliestRetry 返回输掉仲裁后允许重新发起 START 的最早相位 0。
func (m *Master) EarliestRetry() int { return m.busFreeAt }

// QueueWrite 编排一帧或多帧；多帧之间自动插入重复 START。
func (m *Master) QueueWrite(frames ...Frame) {
	m.result.Frames = frames
	m.segs = append(m.segs, seg{kind: segStart, label: "S"})
	for fi, f := range frames {
		if fi > 0 {
			m.segs = append(m.segs, seg{kind: segRStart, label: "Sr"})
		}
		addr := (f.Addr & 0x7f) << 1 // R/W# = 0（写）
		m.appendByte(addr, fmt.Sprintf("ADDR 0x%02x+W", f.Addr))
		for _, d := range f.Data {
			m.appendByte(d, fmt.Sprintf("DATA 0x%02x", d))
		}
	}
	m.segs = append(m.segs, seg{kind: segStop, label: "P"})
}

func (m *Master) appendByte(v byte, label string) {
	for i := 7; i >= 0; i-- {
		lv := High
		if v&(1<<uint(i)) == 0 {
			lv = Low
		}
		m.segs = append(m.segs, seg{kind: segBit, val: lv, label: label})
	}
	m.segs = append(m.segs, seg{kind: segAck, label: label + " ACK"})
}

func (m *Master) Drive(tick, phase int) Drive {
	m.activate(tick)
	if !m.active || m.lost || m.blocked || tick <= m.start+m.stall {
		return Release
	}
	_, current, ok := m.segmentAt(tick)
	if !ok {
		return Release
	}
	logicalPhase := (m.logicalTick(tick) - 1 - m.start) % 4
	scl, sda := bitDrive(current, logicalPhase)
	return Drive{SCL: scl, SDA: sda}
}

func bitDrive(s seg, phase int) (scl, sda Level) {
	switch s.kind {
	case segStart:
		// SCL 维持高，SDA 在相位 1 拉低形成 START，相位 3 拉低 SCL。
		scl = High
		sda = High
		if phase >= 1 {
			sda = Low
		}
		if phase == 3 {
			scl = Low
		}
	case segRStart:
		// 进入时 SCL 已低；SCL 抬高后在 SCL 高期间拉低 SDA。
		scl, sda = Low, High
		if phase >= 1 {
			scl = High
		}
		if phase >= 2 {
			sda = Low
		}
		if phase == 3 {
			scl = Low
		}
	case segStop:
		scl, sda = Low, Low
		if phase >= 1 {
			scl = High
		}
		if phase >= 2 {
			sda = High
		}
	case segAck:
		// 主设备释放 SDA，由从设备拉低应答；仅按时钟驱动 SCL。
		sda = High
		if phase == 0 || phase == 3 {
			scl = Low
		} else {
			scl = High
		}
	default: // segBit
		sda = s.val
		if phase == 0 || phase == 3 {
			scl = Low
		} else {
			scl = High
		}
	}
	return
}

func (m *Master) Observe(s Snapshot) {
	if s.StopCondition() {
		boundary := s.Tick - s.Phase
		m.busFreeAt = boundary + 4
	}

	if !m.active || m.lost || m.blocked || s.Tick <= m.start+m.stall {
		return
	}
	idx, current, ok := m.segmentAt(s.Tick)
	if !ok {
		return
	}
	logicalPhase := (m.logicalTick(s.Tick) - 1 - m.start) % 4

	// 主设备在相位 1 释放 SCL，真实线路必须由线与结果决定。若仍为低，本
	// 逻辑相位不前进，直到从设备释放或超时。
	wantsHighSCL := logicalPhase == 1 || (current.kind == segStart && logicalPhase < 3) ||
		(current.kind == segRStart && logicalPhase >= 1 && logicalPhase < 3) ||
		(current.kind == segStop && logicalPhase >= 1)
	if wantsHighSCL && s.SCL == Low {
		if m.waitSince == 0 {
			m.waitSince = s.Tick
			s.Bus.EnqueueEvent(EvClockWait, m.id, fmt.Sprintf("tick=%d segment=%s", s.Tick, current.label))
		}
		if m.timeout > 0 && s.Tick-m.waitSince+1 >= m.timeout {
			s.Bus.EnqueueEvent(EvClockTimeout, m.id, fmt.Sprintf("tick=%d waited=%d", s.Tick, s.Tick-m.waitSince+1))
			m.abort("clock stretch timeout")
		}
		m.stall++
		return
	}
	if m.waitSince > 0 && s.SCL == High {
		m.waitSince = 0
	}

	// 在应答位的高电平相位中点采样：低=ACK，高=NACK。
	if current.kind == segAck && logicalPhase == 1 && s.SCL == High {
		if s.SDA == High {
			// 从设备无响应：记录错误，后续字节与帧全部裁掉，仅保留 STOP，
			// 从而正常结束而不是卡在等待。
			m.result.Err = fmt.Errorf("%s: NACK at %s (no device responding)", m.id, current.label)
			m.segs = append(m.segs[:idx+1], seg{kind: segStop, label: "P"})
		}
	}
}

func (m *Master) OnEvent(kind, who, detail string) {
	switch kind {
	case EvArbLost:
		if who == m.id {
			m.lose()
		}
	case EvReset:
		m.abort("reset")
	case EvRecoverFailed:
		m.blocked = true
		m.attempts = append(m.attempts, m.result)
		m.active = false
		m.segs = nil
		m.done = false
		m.result.Err = fmt.Errorf("%s: aborted by failed bus recovery at tick %d", m.id, m.b.Now)
		m.finish()
	case EvRecoverDone:
		m.blocked = false
	}
}

func (m *Master) activate(tick int) {
	if m.active || m.done || m.lost || len(m.segs) == 0 || tick < m.start {
		return
	}
	if m.start < tick {
		m.start = tick
	}
	if rem := m.start % 4; rem != 0 {
		m.start += 4 - rem
	}
	if tick < m.start {
		return
	}
	m.active = true
}

func (m *Master) SaveState() interface{} {
	return masterState{
		start: m.start, segs: append([]seg(nil), m.segs...), active: m.active, lost: m.lost,
		done: m.done, result: m.result, attempts: append([]TxResult(nil), m.attempts...),
		lostAt: m.lostAt, busFreeAt: m.busFreeAt, stall: m.stall, waitSince: m.waitSince,
		timeout: m.timeout, blocked: m.blocked,
	}
}

func (m *Master) RestoreState(value interface{}) {
	state, _ := value.(masterState)
	m.start = state.start
	m.segs = append([]seg(nil), state.segs...)
	m.active = state.active
	m.lost = state.lost
	m.done = state.done
	m.result = state.result
	m.result.Frames = append([]Frame(nil), state.result.Frames...)
	m.attempts = append([]TxResult(nil), state.attempts...)
	m.lostAt = state.lostAt
	m.busFreeAt = state.busFreeAt
	m.stall = state.stall
	m.waitSince = state.waitSince
	m.timeout = state.timeout
	m.blocked = state.blocked
}

type masterState struct {
	start     int
	segs      []seg
	active    bool
	lost      bool
	done      bool
	result    TxResult
	attempts  []TxResult
	lostAt    int
	busFreeAt int
	stall     int
	waitSince int
	timeout   int
	blocked   bool
}

func (m *Master) lose() {
	m.attempts = append(m.attempts, m.result)
	m.lost = true
	m.lostAt = m.b.Now
	m.active = false
	m.segs = nil
	m.result.Err = fmt.Errorf("%s: lost arbitration at tick %d; frames retained for retry", m.id, m.b.Now)
	m.finish()
}

// Retry 在总线空闲后重新排队仲裁失败的事务。startTick 必须位于相位 0。
func (m *Master) Retry(startTick int) bool {
	if !m.lost || !m.CanRetryAt(startTick) || (startTick-1)%4 != 0 {
		return false
	}
	frames := m.result.Frames
	m.lost = false
	m.done = false
	m.active = false
	m.start = startTick
	m.result = TxResult{Frames: append([]Frame(nil), frames...)}
	m.QueueWrite(frames...)
	m.done = false
	m.active = false
	return true
}

// TickCommit 由 Runner 在每个总线时钟后调用，检查事务是否走完。
func (m *Master) TickCommit(tick int) {
	m.activate(tick)
	if !m.active || m.done {
		return
	}
	if tick >= m.start+m.stall+len(m.segs)*4 {
		m.active = false
		m.attempts = append(m.attempts, m.result)
		m.finish()
	}
}

func (m *Master) abort(reason string) {
	m.attempts = append(m.attempts, m.result)
	m.active = false
	m.segs = nil
	if !m.done {
		m.result.Err = fmt.Errorf("%s: aborted by %s at tick %d", m.id, reason, m.b.Now)
	}
	m.finish()
}

func (m *Master) finish() {
	if m.done {
		return
	}
	m.done = true
	select {
	case m.doneCh <- m.result:
	default:
	}
}
