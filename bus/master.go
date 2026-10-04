package bus

import (
	"errors"
	"fmt"
)

// Frame 是一次寻址后的一段写传输（教学协议：7 位地址 + 若干写字节）。
type Frame struct {
	Addr byte // 7 位从地址
	Data []byte
}

// Op 是组内的一次相关传输：写若干字节、读若干字节，或先写后读（拆成两个
// Op 表达）。组内各 Op 之间只发重复 START，主机始终占用线路。
type Op struct {
	Addr  byte   // 7 位从地址
	Write []byte // 要写入的字节（纯读操作时为 nil）
	Read  int    // 要读取的字节数（纯写操作时为 0）
}

// OpResult 是组内单个操作的独立结果；组失败不会抹掉已确认操作的结果。
type OpResult struct {
	Op    Op
	Data  []byte // 读操作收到的字节
	Acked bool   // 该操作已被从设备完整确认
	Err   error
}

// TxResult 是一次事务的结果；从设备无响应等情况以错误返回而不是永久等待。
type TxResult struct {
	Frames []Frame
	Ops    []OpResult
	Err    error
}

// MasterStatus 说明主机当前处在等待线路、等待设备还是已失去占用。
type MasterStatus int

const (
	StatusIdle          MasterStatus = iota
	StatusWaitingBus                 // 已排队但总线被占用，等待线路
	StatusDriving                    // 正在驱动线路
	StatusWaitingDevice              // 从设备拉低 SCL 暂停，主机等待设备
	StatusLost                       // 仲裁失败或被中止，已失去占用
	StatusDone
)

func (s MasterStatus) String() string {
	switch s {
	case StatusWaitingBus:
		return "waiting-bus"
	case StatusDriving:
		return "driving"
	case StatusWaitingDevice:
		return "waiting-device"
	case StatusLost:
		return "lost-occupancy"
	case StatusDone:
		return "done"
	default:
		return "idle"
	}
}

// Master 是主设备。它把事务预先编排成若干“位段”，每个位段固定占 4 个
// 四分之一时钟（SCL 的低/上升/高/下降相位）。输掉仲裁时立即丢弃编排、
// 释放总线，绝不继续驱动。
type Master struct {
	id        string
	b         *Bus
	start     int
	segs      []seg
	segOp     []int // 每个位段所属的绝对操作序号，-1 表示不属于任何操作
	active    bool
	lost      bool
	done      bool
	result    TxResult
	doneCh    chan TxResult
	attempts  []TxResult
	lostAt    int
	busFreeAt int

	ops        []Op
	opResults  []OpResult
	opEndSeg   []int // 每个绝对操作最后一个位段的下标
	confirmed  int   // 已确认的前缀操作数（操作按序确认）
	delay      int   // 从设备时钟拉伸累计插入的拍数（始终为 4 的倍数）
	stretching bool
	readShift  byte
	readCnt    int

	exceptionPending bool
}

type segKind int

const (
	segStart segKind = iota
	segRStart
	segBit       // SDA 在整个位单元保持 val
	segAck       // 主设备释放 SDA，读从设备应答
	segReadBit   // 读操作：主设备释放 SDA，采样从设备数据位
	segMasterAck // 读操作：主设备回 ACK（val=Low）或 NACK（val=High）
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

func (m *Master) ID() string { return m.id }

// Done 在事务结束（成功、NACK、仲裁失败均算结束）时可读。
func (m *Master) Done() <-chan TxResult  { return m.doneCh }
func (m *Master) Result() TxResult       { return m.result }
func (m *Master) Active() bool           { return m.active }
func (m *Master) Lost() bool             { return m.lost }
func (m *Master) LostAt() int            { return m.lostAt }
func (m *Master) PendingFrames() []Frame { return append([]Frame(nil), m.result.Frames...) }
func (m *Master) Attempts() []TxResult   { return append([]TxResult(nil), m.attempts...) }

// ConfirmedOps 返回本组中已被从设备完整确认的前缀操作数。
func (m *Master) ConfirmedOps() int { return m.confirmed }

// ExceptionPending 报告是否有异常处置请求在等待本组完成。
func (m *Master) ExceptionPending() bool { return m.exceptionPending }

// Status 报告主机当前状态：等待线路、等待设备、已失去占用等。
func (m *Master) Status() MasterStatus {
	switch {
	case m.lost:
		return StatusLost
	case m.done:
		return StatusDone
	case m.stretching:
		return StatusWaitingDevice
	case m.active:
		return StatusDriving
	case len(m.segs) > 0:
		return StatusWaitingBus
	default:
		return StatusIdle
	}
}

func (m *Master) ArbitratingAt(tick, phase int) bool {
	if !m.active || m.lost || tick < m.start {
		return false
	}
	idx := (tick - 1 - m.start - m.delay) / 4
	return idx >= 0 && idx < len(m.segs) && m.segs[idx].kind == segBit && phase == 1
}

func (m *Master) DataBitAt(tick, phase int) bool {
	if !m.active || m.lost || tick < m.start {
		return false
	}
	idx := (tick - 1 - m.start - m.delay) / 4
	return idx >= 0 && idx < len(m.segs) && m.segs[idx].kind == segBit
}

func (m *Master) ParticipatingAt(tick int) bool {
	return m.active && !m.lost && tick >= m.start
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
	ops := make([]Op, len(frames))
	for i, f := range frames {
		ops[i] = Op{Addr: f.Addr, Write: f.Data}
	}
	m.QueueGroup(ops...)
}

// QueueGroup 编排一组相关传输：组内各操作之间只发重复 START，主机全程
// 占用线路，直到最后一个操作结束才发 STOP。每个操作保留独立结果。
func (m *Master) QueueGroup(ops ...Op) {
	base := len(m.ops)
	m.ops = append(m.ops, ops...)
	for _, op := range ops {
		m.opResults = append(m.opResults, OpResult{Op: op})
	}
	segs, segOp, opEnd := buildSegs(ops, base)
	m.segs = append(m.segs, segs...)
	m.segOp = append(m.segOp, segOp...)
	m.opEndSeg = append(m.opEndSeg, opEnd...)
}

// buildSegs 把一组操作编排成位段：START 开头、STOP 结尾，操作之间重复
// START。返回位段、位段到绝对操作序号的映射和每个操作的结束位段下标。
func buildSegs(ops []Op, base int) ([]seg, []int, []int) {
	segs := []seg{{kind: segStart, label: "S"}}
	segOp := []int{-1}
	opEnd := make([]int, len(ops))
	for i, op := range ops {
		abs := base + i
		if i > 0 {
			segs = append(segs, seg{kind: segRStart, label: "Sr"})
			segOp = append(segOp, abs)
		}
		rw := "W"
		addr := (op.Addr & 0x7f) << 1
		if op.Read > 0 {
			rw = "R"
			addr |= 1
		}
		segs, segOp = appendByteSegs(segs, segOp, abs, addr, fmt.Sprintf("ADDR 0x%02x+%s", op.Addr, rw))
		for _, d := range op.Write {
			segs, segOp = appendByteSegs(segs, segOp, abs, d, fmt.Sprintf("DATA 0x%02x", d))
		}
		for k := 0; k < op.Read; k++ {
			label := fmt.Sprintf("READ #%d", k)
			for j := 0; j < 8; j++ {
				segs = append(segs, seg{kind: segReadBit, label: label})
				segOp = append(segOp, abs)
			}
			ack := High // 最后一字节回 NACK，其余回 ACK
			if k < op.Read-1 {
				ack = Low
			}
			segs = append(segs, seg{kind: segMasterAck, val: ack, label: label + " MACK"})
			segOp = append(segOp, abs)
		}
		opEnd[i] = len(segs) - 1
	}
	segs = append(segs, seg{kind: segStop, label: "P"})
	segOp = append(segOp, -1)
	return segs, segOp, opEnd
}

func appendByteSegs(segs []seg, segOp []int, abs int, v byte, label string) ([]seg, []int) {
	for i := 7; i >= 0; i-- {
		lv := High
		if v&(1<<uint(i)) == 0 {
			lv = Low
		}
		segs = append(segs, seg{kind: segBit, val: lv, label: label})
		segOp = append(segOp, abs)
	}
	segs = append(segs, seg{kind: segAck, label: label + " ACK"})
	segOp = append(segOp, abs)
	return segs, segOp
}

func (m *Master) Drive(tick, phase int) Drive {
	m.activate(tick)
	if !m.active || m.lost || tick < m.start {
		return Release
	}
	if m.stretching {
		// 从设备拉低 SCL 暂停：主机释放时钟、保持数据位，等待设备接续。
		idx := (tick - 1 - m.start - m.delay) / 4
		if idx < 0 || idx >= len(m.segs) {
			return Release
		}
		_, sda := bitDrive(m.segs[idx], 2)
		return Drive{SCL: High, SDA: sda}
	}
	idx := (tick - 1 - m.start - m.delay) / 4
	if idx < 0 || idx >= len(m.segs) {
		return Release
	}
	scl, sda := bitDrive(m.segs[idx], phase)
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
	case segAck, segReadBit:
		// 主设备释放 SDA，由从设备拉低应答；仅按时钟驱动 SCL。
		sda = High
		if phase == 0 || phase == 3 {
			scl = Low
		} else {
			scl = High
		}
	default: // segBit 与 segMasterAck：SDA 保持 val
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

	if !m.active || m.lost || s.Tick < m.start {
		return
	}
	raw := s.Tick - 1 - m.start - m.delay
	if raw < 0 {
		return
	}
	idx := raw / 4
	phase := raw % 4
	if idx < 0 || idx >= len(m.segs) {
		return
	}
	current := m.segs[idx]

	if m.stretching {
		// 从设备释放 SCL 后把延迟对齐到整位边界，本位重新开始。
		if s.SCL == High {
			m.stretching = false
			m.delay = (m.delay + 3) / 4 * 4
			return
		} else {
			m.delay++
			return
		}
	}
	// 本位应释放 SCL 却观察到低电平：从设备在拉伸时钟，进入等待设备状态。
	if !m.stretching && phase == 1 && s.SCL == Low {
		if scl, _ := bitDrive(current, 1); scl == High {
			m.stretching = true
			m.delay++
			return
		}
	}

	opIdx := -1
	if idx < len(m.segOp) {
		opIdx = m.segOp[idx]
	}

	switch {
	case current.kind == segAck && phase == 1:
		// 在应答位的高电平相位中点采样：低=ACK，高=NACK。
		if s.SDA == High {
			// 从设备无响应：记录错误，后续字节与帧全部裁掉，仅保留 STOP，
			// 从而正常结束而不是卡在等待。
			if opIdx >= 0 {
				m.opResults[opIdx].Err = fmt.Errorf("NACK at %s", current.label)
				for i := opIdx + 1; i < len(m.opResults); i++ {
					m.opResults[i].Err = errors.New("skipped after NACK")
				}
			}
			m.result.Err = fmt.Errorf("%s: NACK at %s (no device responding)", m.id, current.label)
			m.segs = append(m.segs[:idx+1], seg{kind: segStop, label: "P"})
		}
	case current.kind == segReadBit && phase == 1:
		bit := byte(0)
		if s.SDA == High {
			bit = 1
		}
		m.readShift = m.readShift<<1 | bit
		m.readCnt++
		if m.readCnt == 8 {
			if opIdx >= 0 {
				m.opResults[opIdx].Data = append(m.opResults[opIdx].Data, m.readShift)
			}
			m.readCnt = 0
			m.readShift = 0
		}
	}

	// 操作的最后一个位段走完且无错误：该操作确认完成，结果独立保留。
	if phase == 3 && opIdx >= 0 && opIdx < len(m.opEndSeg) && idx == m.opEndSeg[opIdx] &&
		m.opResults[opIdx].Err == nil && !m.opResults[opIdx].Acked {
		m.opResults[opIdx].Acked = true
		m.confirmed = opIdx + 1
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
	case EvException:
		if detail == "abort" {
			m.truncateGroup("aborted by higher-priority exception")
		} else {
			m.exceptionPending = true
		}
	}
}

func (m *Master) activate(tick int) {
	if m.active || m.done || m.lost || len(m.segs) == 0 || tick < m.start {
		return
	}
	// 总线被占用时等待线路，直到空闲才发起 START。
	if m.b.Busy() {
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

func (m *Master) lose() {
	m.attempts = append(m.attempts, m.result)
	m.lost = true
	m.lostAt = m.b.Now
	m.active = false
	m.segs = nil
	m.stretching = false
	m.delay = 0
	m.markUnconfirmed(errors.New("lost arbitration"))
	m.result.Err = fmt.Errorf("%s: lost arbitration at tick %d; frames retained for retry", m.id, m.b.Now)
	m.finish()
}

// markUnconfirmed 给尚未确认的操作记录错误；已确认操作的结果保持不动。
func (m *Master) markUnconfirmed(err error) {
	for i := m.confirmed; i < len(m.opResults); i++ {
		if m.opResults[i].Err == nil {
			m.opResults[i].Err = err
		}
	}
}

// truncateGroup 在当前操作边界截断本组：当前操作走完后直接发 STOP，
// 后续操作标记为放弃，已确认操作的结果保留。
func (m *Master) truncateGroup(reason string) {
	err := errors.New(reason)
	if !m.active {
		m.markUnconfirmed(err)
		m.segs = nil
		m.result.Err = fmt.Errorf("%s: %s", m.id, reason)
		m.finish()
		return
	}
	raw := m.b.Now - 1 - m.start - m.delay
	if raw < 0 {
		raw = 0
	}
	idx := raw / 4
	if idx >= len(m.segs) {
		return
	}
	opIdx := -1
	if idx < len(m.segOp) {
		opIdx = m.segOp[idx]
	}
	end := idx
	if opIdx >= 0 && opIdx < len(m.opEndSeg) {
		end = m.opEndSeg[opIdx]
		for i := opIdx + 1; i < len(m.opResults); i++ {
			m.opResults[i].Err = err
		}
	}
	truncated := append([]seg(nil), m.segs[:end+1]...)
	m.segs = append(truncated, seg{kind: segStop, label: "P"})
	m.result.Err = fmt.Errorf("%s: %s", m.id, reason)
}

// AbortGroup 人工放弃剩余操作：当前操作完成后发 STOP 让出线路。
func (m *Master) AbortGroup(reason string) {
	m.truncateGroup(reason)
}

// Retry 在总线空闲后重新排队仲裁失败的事务。startTick 必须位于相位 0。
// 已确认的写入不会重发：只从未确认的第一个操作继续。
func (m *Master) Retry(startTick int) bool {
	if !m.lost || !m.CanRetryAt(startTick) || (startTick-1)%4 != 0 {
		return false
	}
	frames := m.result.Frames
	m.lost = false
	m.done = false
	m.active = false
	m.start = startTick
	m.delay = 0
	m.stretching = false
	m.readCnt = 0
	m.readShift = 0
	m.segs = nil
	m.segOp = nil
	m.opEndSeg = nil
	for i := m.confirmed; i < len(m.opResults); i++ {
		m.opResults[i].Err = nil
		m.opResults[i].Data = nil
	}
	m.result = TxResult{Frames: append([]Frame(nil), frames...)}
	segs, segOp, opEnd := buildSegs(m.ops[m.confirmed:], m.confirmed)
	m.segs = segs
	m.segOp = segOp
	m.opEndSeg = append(make([]int, m.confirmed), opEnd...)
	return true
}

// TickCommit 由 Runner 在每个总线时钟后调用，检查事务是否走完。
func (m *Master) TickCommit(tick int) {
	m.activate(tick)
	if !m.active || m.done {
		return
	}
	if tick >= m.start+m.delay+len(m.segs)*4 {
		m.active = false
		m.attempts = append(m.attempts, m.result)
		m.finish()
	}
}

func (m *Master) abort(reason string) {
	m.attempts = append(m.attempts, m.result)
	m.active = false
	m.segs = nil
	m.stretching = false
	m.markUnconfirmed(fmt.Errorf("aborted by %s", reason))
	if !m.done {
		m.result.Err = fmt.Errorf("%s: aborted by %s at tick %d", m.id, reason, m.b.Now)
	}
	m.finish()
}

func (m *Master) finish() {
	if m.done {
		return
	}
	m.result.Ops = append([]OpResult(nil), m.opResults...)
	m.done = true
	select {
	case m.doneCh <- m.result:
	default:
	}
}
