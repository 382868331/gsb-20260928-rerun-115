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

// TxResult 是一次事务的结果；从设备无响应等情况以错误返回而不是永久等待。
type TxResult struct {
	Frames []Frame
	Err    error
}

// 常见错误，供演练脚本用 errors.Is 判别。
var (
	ErrArbLost  = errors.New("lost arbitration")
	ErrNACK     = errors.New("no device responding (NACK)")
	ErrReset    = errors.New("aborted by reset")
	ErrWaitBusy = errors.New("bus busy, cannot start within wait limit")
)

// Master 是主设备。它把事务预先编排成若干“位段”，每个位段固定占 4 个
// 四分之一时钟（SCL 的低/上升/高/下降相位）。输掉仲裁时立即丢弃编排、
// 释放总线，绝不继续驱动；仲裁失败时保留事务以便 Retry 重新发起。
type Master struct {
	id    string
	b     *Bus
	start int // 起始 tick（对齐到相位 0）

	queued []Frame // 最近一次排队的事务，仲裁失败时据此重试
	segs   []seg

	active       bool // 本刻是否在驱动/参与事务
	granted      bool // 仲裁已确认本设备为唯一占有者
	lost         bool // 本事务已输掉仲裁（保留到 Retry 时清除）
	waitBusy     bool // 在等待总线空闲后再发起
	waitDeadline int  // 等待空闲的最后 tick
	done         bool

	result TxResult
	doneCh chan TxResult
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
	return &Master{id: id, b: b, start: align4(startTick), doneCh: make(chan TxResult, 1)}
}

func align4(t int) int {
	if t < 0 {
		return 0
	}
	return t + (4-t%4)%4
}

func (m *Master) ID() string { return m.id }

// Done 在事务结束（成功、NACK、仲裁失败、复位、等待超时均算结束）时可读。
func (m *Master) Done() <-chan TxResult { return m.doneCh }
func (m *Master) Result() TxResult      { return m.result }

// Active 表示本设备当前是否在总线上进行事务（输掉仲裁后立即为 false）。
func (m *Master) Active() bool { return m.active }

// Granted 表示仲裁器是否已确认本设备独占总线。两个主设备不可能同时为 true。
func (m *Master) Granted() bool { return m.granted }

// Lost 表示上一次发起输掉了仲裁；Retry 成功发起后清除。
func (m *Master) Lost() bool { return m.lost }

// QueueWrite 编排一帧或多帧；多帧之间自动插入重复 START。
func (m *Master) QueueWrite(frames ...Frame) {
	m.queued = frames
	m.result = TxResult{Frames: frames}
	m.buildSegs(frames)
}

// QueueWriteWhenIdle 在 startTick 之前若总线非空闲则等待；超过 waitTicks 仍
// 非空闲时以 ErrWaitBusy 结束。waitTicks<=0 表示只在发起时刻检查一次。
func (m *Master) QueueWriteWhenIdle(waitTicks int, frames ...Frame) {
	m.QueueWrite(frames...)
	m.waitBusy = true
	m.waitDeadline = m.start + waitTicks
}

func (m *Master) buildSegs(frames []Frame) {
	m.segs = append(m.segs[:0], seg{kind: segStart, label: "S"})
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

// Retry 用保留下来的事务重新发起。仲裁失败后可重试；返回 false 表示
// 当前没有可重试的事务。
func (m *Master) Retry(startTick int) bool {
	if m.done && !m.lost {
		return false
	}
	if len(m.queued) == 0 {
		return false
	}
	m.start = align4(startTick)
	m.segs = nil
	m.result = TxResult{Frames: m.queued}
	m.buildSegs(m.queued)
	m.active = false
	m.granted = false
	m.lost = false
	m.waitBusy = false
	m.done = false
	m.doneCh = make(chan TxResult, 1)
	return true
}

func (m *Master) Drive(tick, phase int) Drive {
	if !m.active || m.lost || tick < m.start {
		return Release
	}
	idx := (tick - m.start) / 4
	if idx < 0 || idx >= len(m.segs) {
		return Release
	}
	scl, sda := bitDrive(m.segs[idx], phase)
	return Drive{SCL: scl, SDA: sda}
}

func bitDrive(s seg, phase int) (scl, sda Level) {
	switch s.kind {
	case segStart:
		// 进入时总线空闲（两线高）；相位 1 拉低 SDA 形成 START，相位 3 拉低 SCL。
		scl = High
		sda = High
		if phase >= 1 {
			sda = Low
		}
		if phase == 3 {
			scl = Low
		}
	case segRStart:
		// 进入时 SCL 低、SDA 低；放 SDA、抬 SCL，SCL 高时拉 SDA，相位 3 压低 SCL。
		scl, sda = Low, Low
		if phase >= 1 {
			sda = High
		}
		if phase >= 2 {
			scl = High
			sda = Low // SCL 高时 SDA 下降 => 重复 START
		}
		if phase == 3 {
			scl = Low
		}
	case segStop:
		// 进入时 SCL 低、SDA 低；抬 SCL，再在 SCL 高时放 SDA 形成 STOP。
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

// Observe 只负责设备本地的电平判断：等待空闲、按位仲裁、ACK 采样。
func (m *Master) Observe(s Snapshot) {
	if m.done {
		return
	}

	// 发起前：等待总线空闲。
	if !m.active && len(m.segs) > 0 && s.Tick == m.start {
		if s.SCL == High && s.SDA == High {
			m.active = true
			m.granted = false
		} else if m.waitBusy {
			if s.Tick >= m.waitDeadline {
				m.fail(fmt.Errorf("%s: %w", m.id, ErrWaitBusy))
				return
			}
			// 保持释放继续等；推迟到下一个位单元相位 0 再检查。
			m.start = align4(s.Tick + 4)
		} else {
			// 未要求等待：仲裁模型下只允许同一相位 0 同步发起，
			// 不同步的发起方由仲裁器在 START 上判负。
			m.active = true
		}
	}

	if !m.active || m.lost || s.Tick < m.start {
		return
	}

	idx := (s.Tick - s.Tick%4 - m.start) / 4
	if idx < 0 || idx >= len(m.segs) {
		return
	}
	cur := m.segs[idx]

	// 按位仲裁：在 SCL 高电平中点（相位 2）把本设备 SDA 意图与线与结果比较。
	// 意图高、总线低，说明有别的主设备在发 0，本设备立即输掉并释放总线。
	// 只在主设备自己驱动 SDA 的数据/地址位比较；ACK 位的 SDA 归从设备，
	// START/重复 START/STOP 段所有主设备波形相同，均不参与逐位比较。
	if s.Phase == 2 && cur.kind == segBit {
		_, wantSDA := bitDrive(cur, s.Phase)
		if wantSDA == High && s.SDA == Low && !m.holdsSDA(s) {
			m.loseLocal(fmt.Errorf("%s: %w", m.id, ErrArbLost))
			return
		}
	}

	// 在应答位的高电平相位中点采样：低=ACK，高=NACK（从设备无响应）。
	if cur.kind == segAck && s.Phase == 2 && m.granted {
		if s.SDA == High {
			m.result.Err = fmt.Errorf("%s: %w at %s", m.id, ErrNACK, cur.label)
			m.segs = append(m.segs[:idx+1], seg{kind: segStop, label: "P"})
		}
	}
}

// holdsSDA 判断当前 SDA 低电平是否由本设备自己造成。
func (m *Master) holdsSDA(s Snapshot) bool {
	for _, id := range s.OwnersSDA {
		if id == m.id {
			return true
		}
	}
	return false
}

func (m *Master) OnEvent(kind, who, detail string) {
	switch kind {
	case EvArbLost:
		if who == m.id && m.active {
			m.loseLocal(fmt.Errorf("%s: %w", m.id, ErrArbLost))
		}
	case EvArbGrant:
		if who == m.id {
			m.granted = true
		}
	case EvReset:
		m.abort(fmt.Errorf("%s: %w", m.id, ErrReset))
	}
}

// loseLocal 立即停止驱动，但保留 queued 事务作为重试依据。
func (m *Master) loseLocal(err error) {
	if m.lost {
		return
	}
	m.lost = true
	m.active = false
	m.granted = false
	m.segs = nil
	m.result.Err = err
	m.finish()
}

// TickCommit 由 Runner 在每个总线时钟后调用，检查事务是否走完。
func (m *Master) TickCommit(tick int) {
	if !m.active || m.done || m.lost {
		return
	}
	if tick >= m.start+len(m.segs)*4-1 {
		m.active = false
		m.granted = false
		m.finish()
	}
}

func (m *Master) fail(err error) {
	m.active = false
	m.segs = nil
	m.result.Err = err
	m.finish()
}

func (m *Master) abort(err error) {
	if m.done {
		return
	}
	m.active = false
	m.granted = false
	m.segs = nil
	if m.result.Err == nil {
		m.result.Err = err
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
