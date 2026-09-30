package bus

// ManualDevice 按 tick 映射驱动线路，用于人工核对每个阶段的线路归属。
type ManualDevice struct {
	id     string
	drive  map[int]Drive
	seen   []Snapshot
	events []string
}

func NewManualDevice(id string, drive map[int]Drive) *ManualDevice {
	return &ManualDevice{id: id, drive: drive}
}

func (d *ManualDevice) ID() string { return d.id }

func (d *ManualDevice) Drive(tick, phase int) Drive {
	if drive, ok := d.drive[tick]; ok {
		return drive
	}
	return Release
}

func (d *ManualDevice) Observe(s Snapshot) {
	d.seen = append(d.seen, s)
}

func (d *ManualDevice) OnEvent(kind, who, detail string) {
	d.events = append(d.events, kind+":"+who+":"+detail)
}

func (d *ManualDevice) Snapshots() []Snapshot { return append([]Snapshot(nil), d.seen...) }
func (d *ManualDevice) Events() []string      { return append([]string(nil), d.events...) }
