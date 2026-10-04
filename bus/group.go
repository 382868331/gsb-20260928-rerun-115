package bus

import "fmt"

// Op 是组内一次独立的读或写操作。写操作给出 Data，读操作给出 Len。
type Op struct {
	Addr byte
	Read bool
	Data []byte
	Len  int
}

// OpResult 保留组内一次操作的独立结果；整组失败不会抹掉已完成部分。
type OpResult struct {
	Op      Op
	AddrAck bool
	Written int    // 已被从设备应答的写字节数
	Read    []byte // 已读回的字节
	Done    bool
	Err     error
}

// GroupResult 是整组结果：逐操作结果加组级错误/中止标记。
type GroupResult struct {
	Ops     []OpResult
	Err     error
	Aborted bool
}

// GroupStatus 说明主设备当前在等待线路、等待设备还是已失去占用。
type GroupStatus int

const (
	StatusWaitingBus    GroupStatus = iota // 等待线路（未获得占用）
	StatusOccupied                         // 占用线路，传输进行中
	StatusWaitingDevice                    // 等待设备（从设备拉伸时钟）
	StatusLost                             // 已失去占用（仲裁失败）
	StatusDone                             // 整组完成
	StatusAborted                          // 被人工或异常中止
)

func (s GroupStatus) String() string {
	switch s {
	case StatusWaitingBus:
		return "waiting-bus"
	case StatusOccupied:
		return "occupied"
	case StatusWaitingDevice:
		return "waiting-device"
	case StatusLost:
		return "lost"
	case StatusDone:
		return "done"
	case StatusAborted:
		return "aborted"
	}
	return "unknown"
}

// Checkpoint 是组内任意一位上的保存点：记录已确认进度，供稍后继续。
// 继续时按字节粒度重发未确认部分，已被应答的写入不会重发。
type Checkpoint struct {
	Tick    int
	Results []OpResult
}

// gseg 是组传输的一个位段，带有所属操作与读写元数据。
type gseg struct {
	seg
	op   int  // 在 result.Ops 中的下标；-1 表示 START/STOP 等
	addr bool // 是否为地址字节
	read bool // 是否为读数据位（主设备释放 SDA）
}

// GroupMaster 在获得线路后连续完成一组相关读写操作，中间只发重复
// START、不发 STOP，保持占用避免其他主设备插入。支持从设备时钟拉伸
// （等待设备）、逐操作独立结果、断点续传、任意位保存/继续和人工中止。
type GroupMaster struct {
	id    string
	b     *Bus
	start int // <0 表示等总线空闲满一个位周期后自动开始
	ops   []Op
	segs  []gseg

	stretch       int // 从设备拉伸累计顺延的拍数
	active        bool
	lost          bool
	done          bool
	aborted       bool
	waitingDevice bool
	sawStretch    bool
	idleSeen      int

	result    GroupResult
	doneCh    chan GroupResult
	lostAt    int
	busFreeAt int
	interrupt string

	readShifter byte
	readBits    int
}

func NewGroupMaster(id string, b *Bus, startTick int) *GroupMaster {
	return &GroupMaster{id: id, b: b, start: startTick, doneCh: make(chan GroupResult, 16), busFreeAt: -1}
}

func (g *GroupMaster) ID() string { return g.id }

// QueueGroup 编排一组操作；组内操作之间自动插入重复 START。
func (g *GroupMaster) QueueGroup(ops ...Op) {
	g.ops = append([]Op(nil), ops...)
	g.result = GroupResult{Ops: make([]OpResult, len(ops))}
	for i, op := range ops {
		g.result.Ops[i].Op = op
	}
	g.build(g.ops, 0)
}

func (g *GroupMaster) build(ops []Op, base int) {
	g.segs = nil
	g.addSeg(gseg{seg: seg{kind: segStart, label: "S"}, op: -1})
	for i, op := range ops {
		oi := base + i
		if i > 0 {
			g.addSeg(gseg{seg: seg{kind: segRStart, label: "Sr"}, op: -1})
		}
		addr := (op.Addr & 0x7f) << 1
		rw := "W"
		if op.Read {
			addr |= 1
			rw = "R"
		}
		g.appendGroupByte(addr, oi, true, fmt.Sprintf("ADDR 0x%02x+%s", op.Addr, rw))
		if op.Read {
			for j := 0; j < op.Len; j++ {
				for bit := 7; bit >= 0; bit-- {
					g.addSeg(gseg{seg: seg{kind: segBit, val: High, label: fmt.Sprintf("READ%d", j)}, op: oi, read: true})
				}
				ackVal := Low
				label := fmt.Sprintf("READ%d MACK", j)
				if j == op.Len-1 {
					ackVal = High // 最后一字节以 NACK 结束读
					label = fmt.Sprintf("READ%d MNACK", j)
				}
				g.addSeg(gseg{seg: seg{kind: segMAck, val: ackVal, label: label}, op: oi})
			}
		} else {
			for j, d := range op.Data {
				g.appendGroupByte(d, oi, false, fmt.Sprintf("DATA%d 0x%02x", j, d))
			}
		}
	}
	g.addSeg(gseg{seg: seg{kind: segStop, label: "P"}, op: -1})
}

func (g *GroupMaster) appendGroupByte(v byte, op int, addr bool, label string) {
	for i := 7; i >= 0; i-- {
		lv := High
		if v&(1<<uint(i)) == 0 {
			lv = Low
		}
		g.addSeg(gseg{seg: seg{kind: segBit, val: lv, label: label}, op: op, addr: addr})
	}
	g.addSeg(gseg{seg: seg{kind: segAck, label: label + " ACK"}, op: op, addr: addr})
}

func (g *GroupMaster) addSeg(s gseg) { g.segs = append(g.segs, s) }

// Done 在整组结束（成功、NACK、中止、仲裁失败）时可读。
func (g *GroupMaster) Done() <-chan GroupResult { return g.doneCh }

// Result 返回当前结果的深拷贝，已完成操作的部分进度也包含在内。
func (g *GroupMaster) Result() GroupResult { return copyGroupResult(g.result) }

func (g *GroupMaster) Status() GroupStatus {
	switch {
	case g.lost:
		return StatusLost
	case g.done:
		if g.aborted {
			return StatusAborted
		}
		return StatusDone
	case !g.active:
		return StatusWaitingBus
	case g.waitingDevice:
		return StatusWaitingDevice
	default:
		return StatusOccupied
	}
}

func (g *GroupMaster) Lost() bool   { return g.lost }
func (g *GroupMaster) LostAt() int  { return g.lostAt }
func (g *GroupMaster) Active() bool { return g.active }

// SawStretch 报告本组是否经历过从设备时钟拉伸。
func (g *GroupMaster) SawStretch() bool { return g.sawStretch }

func (g *GroupMaster) CanRetryAt(tick int) bool {
	return g.lost && g.busFreeAt >= 0 && tick >= g.busFreeAt
}

func (g *GroupMaster) EarliestRetry() int { return g.busFreeAt }

// Save 在组内任意一位上保存进度；已完成与已确认的字节都进入保存点。
func (g *GroupMaster) Save() Checkpoint {
	return Checkpoint{Tick: g.b.Now, Results: copyGroupResult(g.result).Ops}
}

// ResumeAt 从保存点继续：跳过已完成操作与已确认字节，从未确认的字节
// 重新开始（新的 START），已确认写入不会重发。
func (g *GroupMaster) ResumeAt(cp Checkpoint, startTick int) {
	g.result.Ops = copyGroupResult(GroupResult{Ops: cp.Results}).Ops
	g.resumeFromResults(startTick)
}

// Retry 在仲裁失败并等到空闲位周期后，从已确认断点继续而不是整组重发。
func (g *GroupMaster) Retry(startTick int) bool {
	if !g.lost || !g.CanRetryAt(startTick) || (startTick-1)%4 != 0 {
		return false
	}
	g.resumeFromResults(startTick)
	return true
}

func (g *GroupMaster) resumeFromResults(startTick int) {
	k := 0
	for k < len(g.result.Ops) && g.result.Ops[k].Done {
		k++
	}
	if k == len(g.result.Ops) {
		g.finish()
		return
	}
	remaining := []Op{}
	for i := k; i < len(g.ops); i++ {
		op := g.ops[i]
		if i == k {
			res := g.result.Ops[k]
			if op.Read {
				op.Len -= len(res.Read)
			} else {
				op.Data = op.Data[res.Written:]
			}
		}
		remaining = append(remaining, op)
	}
	g.lost = false
	g.done = false
	g.aborted = false
	g.active = false
	g.stretch = 0
	g.waitingDevice = false
	g.readBits = 0
	g.readShifter = 0
	g.start = startTick
	g.result.Err = nil
	g.result.Aborted = false
	g.build(remaining, k)
}

// Interrupt 是更高优先级异常处置的入口：在当前位段边界截断整组，
// 以 STOP 正常释放线路，已完成操作的结果保留。
func (g *GroupMaster) Interrupt(reason string) {
	if !g.active {
		g.aborted = true
		g.result.Aborted = true
		g.result.Err = fmt.Errorf("%s: interrupted before acquiring bus: %s", g.id, reason)
		g.finish()
		return
	}
	g.interrupt = reason
}

func (g *GroupMaster) Drive(tick, phase int) Drive {
	g.activate(tick)
	if !g.active || g.lost || g.start < 0 || tick < g.start {
		return Release
	}
	eff := tick - g.stretch
	idx := (eff - 1 - g.start) / 4
	if idx < 0 || idx >= len(g.segs) {
		return Release
	}
	ephase := (eff - 1 - g.start) % 4
	scl, sda := bitDrive(g.segs[idx].seg, ephase)
	return Drive{SCL: scl, SDA: sda}
}

func (g *GroupMaster) Observe(s Snapshot) {
	if s.SCL == High && s.SDA == High {
		g.idleSeen++
	} else {
		g.idleSeen = 0
	}
	if s.StopCondition() {
		boundary := s.Tick - s.Phase
		g.busFreeAt = boundary + 4
	}
	if !g.active || g.lost || g.start < 0 || s.Tick < g.start {
		return
	}
	eff := s.Tick - g.stretch
	idx := (eff - 1 - g.start) / 4
	ephase := (eff - 1 - g.start) % 4
	if idx < 0 || idx >= len(g.segs) {
		return
	}
	cur := g.segs[idx]

	// 从设备拉伸时钟：本相位 SCL 应升高却被拉低，整组时序顺延一拍，
	// 状态翻转为“等待设备”，直到 SCL 被释放。
	if ephase == 1 && s.SCL == Low && (cur.kind == segBit || cur.kind == segAck || cur.kind == segMAck) {
		g.stretch++
		g.waitingDevice = true
		g.sawStretch = true
		return
	}

	// 人工中止：在当前位段边界截断，补 STOP 释放线路。
	if g.interrupt != "" && ephase == 3 && cur.kind != segStop {
		g.segs = append(g.segs[:idx+1], gseg{seg: seg{kind: segStop, label: "P"}, op: -1})
		g.aborted = true
		g.result.Aborted = true
		g.result.Err = fmt.Errorf("%s: interrupted: %s", g.id, g.interrupt)
		g.interrupt = ""
		return
	}

	if ephase != 1 || s.SCL != High {
		return
	}
	g.waitingDevice = false

	switch {
	case cur.kind == segAck:
		res := &g.result.Ops[cur.op]
		if s.SDA == Low {
			if cur.addr {
				res.AddrAck = true
			} else {
				res.Written++
				if res.Written == len(res.Op.Data) {
					res.Done = true
				}
			}
		} else {
			res.Err = fmt.Errorf("%s: NACK at %s (device quit or absent)", g.id, cur.label)
			g.endGroup(idx, fmt.Errorf("op %d NACK at %s", cur.op, cur.label))
		}
	case cur.kind == segBit && cur.read:
		bit := byte(0)
		if s.SDA == High {
			bit = 1
		}
		g.readShifter = (g.readShifter << 1) | bit
		g.readBits++
		if g.readBits == 8 {
			g.readBits = 0
			res := &g.result.Ops[cur.op]
			res.Read = append(res.Read, g.readShifter)
			g.readShifter = 0
			if len(res.Read) == res.Op.Len {
				res.Done = true
			}
		}
	}
}

// endGroup 在从设备退出（NACK）时截断整组：补 STOP，保留已完成结果。
func (g *GroupMaster) endGroup(idx int, err error) {
	g.segs = append(g.segs[:idx+1], gseg{seg: seg{kind: segStop, label: "P"}, op: -1})
	g.result.Err = err
}

func (g *GroupMaster) OnEvent(kind, who, detail string) {
	switch kind {
	case EvArbLost:
		if who == g.id {
			g.lose()
		}
	case EvReset:
		g.abort("reset")
	}
}

func (g *GroupMaster) lose() {
	g.lost = true
	g.lostAt = g.b.Now
	g.active = false
	g.segs = nil
	g.result.Err = fmt.Errorf("%s: lost arbitration at tick %d; acknowledged progress retained", g.id, g.b.Now)
	g.finish()
}

func (g *GroupMaster) abort(reason string) {
	g.active = false
	g.segs = nil
	g.aborted = true
	g.result.Aborted = true
	if g.result.Err == nil {
		g.result.Err = fmt.Errorf("%s: aborted by %s at tick %d; partial results kept", g.id, reason, g.b.Now)
	}
	g.finish()
}

func (g *GroupMaster) activate(tick int) {
	if g.active || g.done || g.lost || len(g.segs) == 0 {
		return
	}
	if g.start >= 0 && tick < g.start {
		return
	}
	if g.start < 0 {
		// 等待线路：总线连续空闲一个位周期后才允许发起 START。
		if g.idleSeen < 4 {
			return
		}
		g.start = tick
	}
	if g.start < tick {
		g.start = tick
	}
	if rem := g.start % 4; rem != 0 {
		g.start += 4 - rem
	}
	if tick < g.start {
		return
	}
	g.active = true
}

// TickCommit 由 Runner 在每个总线时钟后调用，检查组是否走完。
func (g *GroupMaster) TickCommit(tick int) {
	g.activate(tick)
	if !g.active || g.done {
		return
	}
	if tick >= g.start+len(g.segs)*4+g.stretch {
		g.active = false
		g.finish()
	}
}

func (g *GroupMaster) finish() {
	if g.done {
		return
	}
	g.done = true
	g.active = false
	select {
	case g.doneCh <- copyGroupResult(g.result):
	default:
	}
}

// ParticipatingAt / DataBitAt 实现仲裁器 Participant 接口。
// 读数据位与主设备应答位不参与仲裁比较。
func (g *GroupMaster) ParticipatingAt(tick int) bool {
	return g.active && !g.lost && g.start >= 0 && tick >= g.start
}

func (g *GroupMaster) DataBitAt(tick, phase int) bool {
	if !g.ParticipatingAt(tick) {
		return false
	}
	eff := tick - g.stretch
	idx := (eff - 1 - g.start) / 4
	if idx < 0 || idx >= len(g.segs) {
		return false
	}
	cur := g.segs[idx]
	return cur.kind == segBit && !cur.read
}

func copyGroupResult(in GroupResult) GroupResult {
	out := in
	out.Ops = make([]OpResult, len(in.Ops))
	for i, r := range in.Ops {
		r.Read = append([]byte(nil), r.Read...)
		out.Ops[i] = r
	}
	return out
}
