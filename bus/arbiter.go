package bus

import (
	"fmt"
	"sort"
)

// Arbiter 在每个快照上比较主设备“想发送的 SDA”和线与后的 SDA。
// 输掉的主设备在该相位观察结束后立即停止驱动；START/STOP 不参与比较。
type Arbiter struct {
	id       string
	masters  map[string]*Master
	suppress map[*Recovery]bool
	active   map[string]bool
	granted  map[string]bool
}

type arbiterState struct {
	active  map[string]bool
	granted map[string]bool
}

func NewArbiter(id string, masters ...*Master) *Arbiter {
	a := &Arbiter{id: id, masters: map[string]*Master{}, suppress: map[*Recovery]bool{}, active: map[string]bool{}, granted: map[string]bool{}}
	for _, master := range masters {
		a.masters[master.ID()] = master
	}
	return a
}

func (a *Arbiter) SuppressRecovery(recoveries ...*Recovery) {
	for _, recovery := range recoveries {
		a.suppress[recovery] = true
	}
}

func (a *Arbiter) ID() string                  { return a.id }
func (a *Arbiter) Drive(tick, phase int) Drive { return Release }

func (a *Arbiter) Observe(s Snapshot) {
	participating := map[string]bool{}
	for id, master := range a.masters {
		if master.ParticipatingAt(s.Tick) {
			participating[id] = true
			a.active[id] = true
		} else {
			participating[id] = false
		}
	}

	for recovery := range a.suppress {
		if recovery.Active() || (!recovery.Done() && s.Tick >= recovery.start) {
			return
		}
	}

	if s.StopCondition() {
		for id := range a.active {
			a.active[id] = false
			a.granted[id] = false
		}
	}

	for id, master := range a.masters {
		drive, ok := s.Drives[id]
		if !ok {
			a.active[id] = false
			a.granted[id] = false
			continue
		}
		if !participating[id] || !master.ArbitratingAt(s.Tick, s.Phase) || !s.SCLRising() {
			continue
		}
		if a.active[id] && drive.SDA == High && s.SDA == Low && a.anotherMasterDrivesSDA(id, s) {
			a.active[id] = false
			a.granted[id] = false
			s.Bus.EnqueueEvent(EvArbLost, id, fmt.Sprintf("tick=%d phase=%d", s.Tick, s.Phase))
		}
	}

	active := make([]string, 0, len(a.active))
	for id, on := range a.active {
		if on {
			active = append(active, id)
		}
	}
	sort.Strings(active)
	if len(active) == 1 && !a.granted[active[0]] {
		a.granted[active[0]] = true
		s.Bus.EnqueueEvent(EvArbGrant, active[0], fmt.Sprintf("tick=%d phase=%d", s.Tick, s.Phase))
	}
}

func (a *Arbiter) anotherMasterDrivesSDA(id string, s Snapshot) bool {
	for _, owner := range s.OwnersSDA {
		if owner != id {
			if _, ok := a.masters[owner]; ok {
				return true
			}
		}
	}
	return false
}

func (a *Arbiter) OnEvent(kind, who, detail string) {
	switch kind {
	case EvArbLost:
		a.active[who] = false
		a.granted[who] = false
	case EvReset, EvRecoverDone, EvStop:
		for id := range a.active {
			a.active[id] = false
			a.granted[id] = false
		}
	}
}

func (a *Arbiter) SaveState() interface{} {
	return arbiterState{active: copyBoolMap(a.active), granted: copyBoolMap(a.granted)}
}

func (a *Arbiter) RestoreState(value interface{}) {
	state, _ := value.(arbiterState)
	a.active = copyBoolMap(state.active)
	a.granted = copyBoolMap(state.granted)
}

func copyBoolMap(in map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
