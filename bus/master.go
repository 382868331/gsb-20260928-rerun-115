package bus

type Status string

const (
	StatusCompleted       Status = "COMPLETED"
	StatusNACK            Status = "NACK"
	StatusArbitrationLost Status = "ARBITRATION_LOST"
	StatusReset           Status = "RESET"
)

type Message struct {
	Addr byte
	Data []byte
}

type Result struct {
	Status Status
	Index  int
}

type Master struct {
	Name       string
	bus        *Bus
	active     bool
	generation int64
	done       func(Result)
	messages   []Message
	msgIndex   int
	byteIndex  int
	bitIndex   int
	ackSlot    bool
	current    byte
}

func NewMaster(name string, bus *Bus) *Master {
	m := &Master{Name: name, bus: bus}
	bus.register(m)
	return m
}

func (m *Master) Active() bool { return m.active }

func (m *Master) Transaction(messages []Message, done func(Result)) {
	if m.active {
		panic("master " + m.Name + ": transaction already active")
	}
	m.active = true
	m.generation = m.bus.Generation()
	m.done = done
	m.messages = append([]Message(nil), messages...)
	m.msgIndex, m.byteIndex, m.bitIndex = 0, 0, 0
	m.ackSlot = false
	m.current = (m.messages[0].Addr & 0x7f) << 1
	m.bus.SDA.Drive(m.Name, true, m.bus.Clock.Now, m.bus)
	m.bus.Clock.Post(1, func(now Unit) {
		if m.live() {
			m.bus.SCL.Drive(m.Name, true, now, m.bus)
			m.beginLow(now)
		}
	})
}

func (m *Master) live() bool { return m.active && m.generation == m.bus.Generation() }

func (m *Master) finish(status Status, now Unit) {
	if !m.active {
		return
	}
	m.active = false
	m.bus.SDA.Release(m.Name, now, m.bus)
	m.bus.SCL.Release(m.Name, now, m.bus)
	if m.done != nil {
		done := m.done
		m.done = nil
		done(Result{Status: status, Index: m.msgIndex})
	}
}

func (m *Master) ForceRelease(now Unit) {
	m.active = false
	m.done = nil
	m.bus.SDA.Release(m.Name, now, m.bus)
	m.bus.SCL.Release(m.Name, now, m.bus)
}

// beginLow starts one SCL-low setup phase. SDA changes only while SCL is low.
func (m *Master) beginLow(now Unit) {
	if m.ackSlot {
		m.bus.SDA.Drive(m.Name, false, now, m.bus)
	} else {
		bitIsHigh := (m.current>>(7-uint(m.bitIndex)))&1 == 1
		m.bus.SDA.Drive(m.Name, !bitIsHigh, now, m.bus)
	}
	half := m.bus.Timing.HalfBit
	m.bus.Clock.Post(half, func(now Unit) {
		if !m.live() {
			return
		}
		m.bus.SCL.Drive(m.Name, false, now, m.bus)
		if !m.ackSlot {
			bitIsHigh := (m.current>>(7-uint(m.bitIndex)))&1 == 1
			if bitIsHigh && m.bus.SDA.Level() == Low {
				m.finish(StatusArbitrationLost, now)
				return
			}
		}
		m.bus.Clock.Post(half, m.endHigh)
	})
}

func (m *Master) endHigh(now Unit) {
	if !m.live() {
		return
	}
	if m.ackSlot {
		// SDA must be released after this falling edge so the slave can own/end ACK.
		m.bus.SCL.Drive(m.Name, false, now, m.bus)
		m.bus.SDA.Drive(m.Name, false, now, m.bus)
		m.ackSlot = false
		m.afterAck(now)
		return
	}
	m.bitIndex++
	if m.bitIndex == 8 {
		m.bitIndex = 0
		m.ackSlot = true
	}
	m.bus.SCL.Drive(m.Name, true, now, m.bus)
	m.beginLow(now)
}

func (m *Master) afterAck(fall Unit) {
	half := m.bus.Timing.HalfBit
	m.bus.Clock.Post(m.bus.Timing.Setup, func(now Unit) {
		if !m.live() {
			return
		}
		acknowledged := m.bus.SDA.Level() == Low
		m.bus.Clock.Post(half-m.bus.Timing.Setup, func(now Unit) { m.finishAck(now, acknowledged) })
	})
}

func (m *Master) finishAck(now Unit, acknowledged bool) {
	if !m.live() {
		return
	}
	m.bus.SCL.Drive(m.Name, true, now, m.bus)
	if !acknowledged {
		m.stop(now, StatusNACK)
		return
	}
	message := m.messages[m.msgIndex]
	if m.byteIndex < len(message.Data) {
		m.current = message.Data[m.byteIndex]
		m.byteIndex++
		m.bitIndex = 0
		m.beginLow(now)
		return
	}
	if m.msgIndex+1 < len(m.messages) {
		m.repeatedStart(now)
		return
	}
	m.stop(now, StatusCompleted)
}

func (m *Master) repeatedStart(now Unit) {
	m.msgIndex++
	m.byteIndex = 0
	m.bitIndex = 0
	m.current = (m.messages[m.msgIndex].Addr & 0x7f) << 1
	half := m.bus.Timing.HalfBit
	m.bus.SDA.Drive(m.Name, false, now, m.bus)
	m.bus.Clock.Post(half, func(now Unit) {
		if !m.live() {
			return
		}
		m.bus.SCL.Drive(m.Name, false, now, m.bus)
		m.bus.Clock.Post(m.bus.Timing.Setup, func(now Unit) {
			if !m.live() {
				return
			}
			m.bus.SDA.Drive(m.Name, true, now, m.bus)
			m.bus.Clock.Post(1, func(now Unit) {
				if m.live() {
					m.bus.SCL.Drive(m.Name, true, now, m.bus)
					m.beginLow(now)
				}
			})
		})
	})
}

func (m *Master) stop(now Unit, status Status) {
	m.bus.SDA.Drive(m.Name, true, now, m.bus)
	m.bus.Clock.Post(m.bus.Timing.HalfBit, func(now Unit) {
		if !m.live() {
			return
		}
		m.bus.SCL.Drive(m.Name, true, now, m.bus)
		m.bus.Clock.Post(m.bus.Timing.Setup, func(now Unit) {
			if m.live() {
				m.finish(status, now)
			}
		})
	})
}
