package bus

import (
	"fmt"
	"sort"
	"strings"
)

const PullUp = "PULLUP"

type Signal int

const (
	SDA Signal = iota
	SCL
)

func (s Signal) String() string {
	if s == SDA {
		return "SDA"
	}
	return "SCL"
}

type Level int

const (
	High Level = iota
	Low
)

func (l Level) String() string {
	if l == Low {
		return "LOW"
	}
	return "HIGH"
}

// Segment records one stable level interval and every active cause. HIGH is
// attributed to the pull-up; LOW lists every open-drain device pulling it low.
type Segment struct {
	Signal Signal
	Level  Level
	Start  Unit
	End    Unit
	Owners []string
}

func (s Segment) String() string {
	return fmt.Sprintf("%s %s t=%d..%d owners=%s",
		s.Signal, s.Level, s.Start, s.End, strings.Join(s.Owners, "+"))
}

type levelWaiter struct {
	want Level
	fn   func()
}

type EdgeObserver func(signal Signal, old, next Level)

type Line struct {
	signal  Signal
	low     map[string]bool
	level   Level
	start   Unit
	waiters []levelWaiter
}

func newLine(signal Signal, now Unit) *Line {
	return &Line{signal: signal, low: map[string]bool{}, level: High, start: now}
}

func (l *Line) Level() Level { return l.level }

func (l *Line) Owners() []string {
	if l.level == High {
		return []string{PullUp}
	}
	owners := make([]string, 0, len(l.low))
	for owner := range l.low {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	return owners
}

func (l *Line) HasLowDriver(owner string) bool { return l.low[owner] }

// Drive turns this owner's open-drain transistor on or off.
func (l *Line) Drive(owner string, low bool, now Unit, bus *Bus) {
	oldOwners := l.Owners()
	if low {
		if l.low[owner] {
			return
		}
		l.low[owner] = true
	} else {
		if !l.low[owner] {
			return
		}
		delete(l.low, owner)
	}
	if l.level == Low {
		if len(l.low) == 0 {
			l.resolve(now, bus, oldOwners)
		} else {
			l.splitIfOwnersChanged(now, bus, oldOwners)
		}
		return
	}
	l.resolve(now, bus, oldOwners)
}

// Release removes one device's contribution. Releasing an idle driver is safe.
func (l *Line) Release(owner string, now Unit, bus *Bus) {
	oldOwners := l.Owners()
	if !l.low[owner] {
		return
	}
	delete(l.low, owner)
	if l.level == Low {
		if len(l.low) == 0 {
			l.resolve(now, bus, oldOwners)
		} else {
			l.splitIfOwnersChanged(now, bus, oldOwners)
		}
		return
	}
	l.resolve(now, bus, oldOwners)
}

func (l *Line) forceReleaseAll(now Unit, bus *Bus) {
	oldOwners := l.Owners()
	if len(l.low) == 0 {
		return
	}
	l.low = map[string]bool{}
	if l.level == Low {
		l.resolve(now, bus, oldOwners)
		return
	}
	l.resolve(now, bus, oldOwners)
}

func (l *Line) splitIfOwnersChanged(now Unit, bus *Bus, oldOwners []string) {
	newOwners := l.Owners()
	if equalOwners(oldOwners, newOwners) {
		return
	}
	if now > l.start {
		bus.segments = append(bus.segments, Segment{
			Signal: l.signal, Level: l.level, Start: l.start, End: now, Owners: oldOwners,
		})
	}
	l.start = now
}

func equalOwners(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (l *Line) resolve(now Unit, bus *Bus, oldOwners []string) {
	next := High
	if len(l.low) > 0 {
		next = Low
	}
	if next == l.level {
		return
	}
	if now > l.start {
		bus.segments = append(bus.segments, Segment{
			Signal: l.signal, Level: l.level, Start: l.start, End: now, Owners: oldOwners,
		})
	}
	old := l.level
	l.level = next
	l.start = now
	bus.notify(l.signal, old, next)
	waiters := l.waiters
	l.waiters = nil
	for _, waiter := range waiters {
		if waiter.want == next {
			waiter.fn()
		} else {
			l.waiters = append(l.waiters, waiter)
		}
	}
}

func (l *Line) close(now Unit, segments *[]Segment) {
	if now > l.start {
		*segments = append(*segments, Segment{
			Signal: l.signal, Level: l.level, Start: l.start, End: now, Owners: l.Owners(),
		})
	}
	l.start = now
}

func (l *Line) waitLevel(level Level, fn func()) {
	if l.level == level {
		fn()
		return
	}
	l.waiters = append(l.waiters, levelWaiter{want: level, fn: fn})
}

type Timing struct {
	HalfBit Unit
	Setup   Unit
}

func DefaultTiming() Timing { return Timing{HalfBit: 10, Setup: 2} }

type Resettable interface {
	ForceRelease(now Unit)
}

// Bus is the virtual electrical medium and deterministic timing source.
type Bus struct {
	Clock      *Clock
	Timing     Timing
	SDA        *Line
	SCL        *Line
	segments   []Segment
	observers  []EdgeObserver
	devices    []Resettable
	generation int64
}

func New(clock *Clock, timing Timing) *Bus {
	bus := &Bus{Clock: clock, Timing: timing, generation: 1}
	bus.SDA = newLine(SDA, clock.Now)
	bus.SCL = newLine(SCL, clock.Now)
	return bus
}

func (b *Bus) register(device Resettable) { b.devices = append(b.devices, device) }

func (b *Bus) notify(signal Signal, old, next Level) {
	for _, observer := range b.observers {
		observer(signal, old, next)
	}
}

// ObserveEdges exposes only resolved electrical events to protocol decoders.
func (b *Bus) ObserveEdges(observer func(signal Signal, old, next Level)) {
	b.observers = append(b.observers, observer)
}

// Segments closes and returns the complete, time-ordered ownership trace.
func (b *Bus) Segments() []Segment {
	b.SDA.close(b.Clock.Now, &b.segments)
	b.SCL.close(b.Clock.Now, &b.segments)
	out := append([]Segment(nil), b.segments...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].Signal < out[j].Signal
	})
	return out
}

// Reset aborts scheduled work and releases every driver immediately.
func (b *Bus) Reset() {
	now := b.Clock.Now
	b.generation++
	b.Clock.queue = nil
	b.SCL.forceReleaseAll(now, b)
	b.SDA.forceReleaseAll(now, b)
	b.SDA.waiters = nil
	b.SCL.waiters = nil
	for _, device := range b.devices {
		device.ForceRelease(now)
	}
}

func (b *Bus) Generation() int64 { return b.generation }
