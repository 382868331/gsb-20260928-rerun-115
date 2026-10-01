package bus

type permanentHolder struct{ id string }

func (p *permanentHolder) ID() string                     { return p.id }
func (p *permanentHolder) Drive(tick, phase int) Drive    { return Drive{SCL: High, SDA: Low} }
func (p *permanentHolder) Observe(Snapshot)               {}
func (p *permanentHolder) OnEvent(string, string, string) {}
