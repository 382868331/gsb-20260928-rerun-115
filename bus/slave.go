package bus

import "fmt"

type Event struct {
	Time Unit
	Kind string
	Addr byte
	Data byte
}

// Slave decodes only resolved SDA/SCL edges and owns only its own drivers.
type Slave struct {
	Name       string
	Addr       byte
	bus        *Bus
	Log        []Event
	selected   bool
	shift      byte
	count      int
	ignoreFall bool
	sclHigh    bool
}

func NewSlave(name string, addr byte, bus *Bus) *Slave {
	s := &Slave{Name: name, Addr: addr & 0x7f, bus: bus, sclHigh: true}
	bus.register(s)
	bus.ObserveEdges(s.onEdge)
	return s
}

func (s *Slave) ForceRelease(now Unit) {
	s.bus.SDA.Release(s.Name, now, s.bus)
	s.bus.SCL.Release(s.Name, now, s.bus)
	s.selected = false
	s.shift = 0
	s.count = 0
	s.ignoreFall = false
	s.sclHigh = true
}

func (s *Slave) onEdge(signal Signal, old, next Level) {
	now := s.bus.Clock.Now
	if signal == SCL {
		s.sclHigh = next == High
		if next == High {
			s.count++
			if s.count <= 8 {
				bit := byte(0)
				if s.bus.SDA.Level() == High {
					bit = 1
				}
				s.shift = s.shift<<1 | bit
			}
		} else if s.ignoreFall {
			s.ignoreFall = false
		} else if s.count == 8 {
			s.ackAfterByte(now)
		} else if s.count == 9 {
			s.shift = 0
			s.count = 0
			s.bus.SDA.Drive(s.Name, false, now, s.bus)
		}
		return
	}
	if signal == SDA && !s.sclHigh {
		return
	}
	if signal == SDA && old == High && next == Low {
		s.Log = append(s.Log, Event{Time: now, Kind: "START"})
		s.selected = false
		s.shift = 0
		s.count = 0
		s.ignoreFall = true
		s.bus.SDA.Drive(s.Name, false, now, s.bus)
		return
	}
	if signal == SDA && old == Low && next == High {
		if s.selected {
			s.Log = append(s.Log, Event{Time: now, Kind: "STOP", Addr: s.Addr})
		}
		s.ForceRelease(now)
		return
	}
}

func (s *Slave) ackAfterByte(now Unit) {
	if s.shift == s.Addr<<1 {
		s.selected = true
		s.Log = append(s.Log, Event{Time: now, Kind: "ADDR_ACK", Addr: s.Addr})
		s.bus.SDA.Drive(s.Name, true, now, s.bus)
	} else if s.selected {
		s.Log = append(s.Log, Event{Time: now, Kind: "DATA", Addr: s.Addr, Data: s.shift})
		s.bus.SDA.Drive(s.Name, true, now, s.bus)
	} else {
		s.bus.SDA.Drive(s.Name, false, now, s.bus)
	}
}

func (s *Slave) String() string {
	return fmt.Sprintf("%s@0x%02x events=%d", s.Name, s.Addr, len(s.Log))
}
