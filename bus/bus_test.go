package bus

import (
	"testing"
)

func newTestBus() (*Clock, *Bus) {
	clock := &Clock{}
	return clock, New(clock, DefaultTiming())
}

func runResult(t *testing.T, clock *Clock, master *Master, messages []Message) Result {
	t.Helper()
	resultCh := make(chan Result, 1)
	master.Transaction(messages, func(result Result) { resultCh <- result })
	clock.Run()
	select {
	case result := <-resultCh:
		return result
	default:
		t.Fatalf("transaction did not finish")
	}
	return Result{}
}

func TestSingleMasterWriteCompletesAndSlaveReceives(t *testing.T) {
	clock, testBus := newTestBus()
	master := NewMaster("M0", testBus)
	slave := NewSlave("S0", 0x23, testBus)

	result := runResult(t, clock, master, []Message{{Addr: 0x23, Data: []byte{0xA5, 0x0F}}})
	if result.Status != StatusCompleted {
		t.Fatalf("status = %s, want COMPLETED", result.Status)
	}
	if len(slave.Log) != 4 {
		t.Fatalf("events = %#v", slave.Log)
	}
	if slave.Log[0].Kind != "START" || slave.Log[1].Kind != "ADDR_ACK" {
		t.Fatalf("unexpected events: %#v", slave.Log)
	}
	if slave.Log[2].Kind != "DATA" || slave.Log[2].Data != 0xA5 ||
		slave.Log[3].Data != 0x0F {
		t.Fatalf("unexpected data events: %#v", slave.Log)
	}
	if testBus.SDA.Level() != High || testBus.SCL.Level() != High {
		t.Fatal("bus was not released after STOP")
	}
}

func TestArbitrationWinnerContinuesAndLoserReleasesBus(t *testing.T) {
	clock, testBus := newTestBus()
	winner := NewMaster("M_WIN", testBus)
	loser := NewMaster("M_LOSE", testBus)
	NewSlave("SENSOR", 0x10, testBus)

	winnerCh := make(chan Result, 1)
	loserCh := make(chan Result, 1)
	winner.Transaction([]Message{{Addr: 0x10, Data: []byte{0x01}}}, func(result Result) {
		winnerCh <- result
	})
	loser.Transaction([]Message{{Addr: 0x20, Data: []byte{0x02}}}, func(result Result) {
		loserCh <- result
	})
	clock.Run()

	loserResult := <-loserCh
	winnerResult := <-winnerCh
	if loserResult.Status != StatusArbitrationLost {
		t.Fatalf("loser status = %s", loserResult.Status)
	}
	if winnerResult.Status != StatusCompleted {
		t.Fatalf("winner status = %s, want COMPLETED", winnerResult.Status)
	}
	if loser.Active() {
		t.Fatal("loser remained active after losing arbitration")
	}
	if testBus.SDA.HasLowDriver("M_LOSE") || testBus.SCL.HasLowDriver("M_LOSE") {
		t.Fatal("loser continued to drive the bus")
	}
}

func TestAbsentSlaveNackEndsWaitNormally(t *testing.T) {
	clock, testBus := newTestBus()
	master := NewMaster("M0", testBus)
	result := runResult(t, clock, master, []Message{{Addr: 0x77, Data: []byte{1}}})
	if result.Status != StatusNACK {
		t.Fatalf("status = %s, want NACK", result.Status)
	}
	if testBus.SDA.Level() != High || testBus.SCL.Level() != High {
		t.Fatal("unmatched address left the bus occupied")
	}
}

func TestRepeatedStartAddressesTwoSlaves(t *testing.T) {
	clock, testBus := newTestBus()
	master := NewMaster("M0", testBus)
	first := NewSlave("S1", 0x11, testBus)
	second := NewSlave("S2", 0x22, testBus)

	result := runResult(t, clock, master, []Message{
		{Addr: 0x11, Data: []byte{0xAA}},
		{Addr: 0x22, Data: []byte{0xBB}},
	})
	if result.Status != StatusCompleted {
		t.Fatalf("status = %s", result.Status)
	}
	starts := 0
	for _, event := range first.Log {
		if event.Kind == "START" {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("first slave saw %d START events, want 2", starts)
	}
	if len(second.Log) < 2 || second.Log[0].Kind != "START" {
		t.Fatalf("second slave missed repeated START: %#v", second.Log)
	}
}

func TestResetDuringTransactionLeavesNoOwner(t *testing.T) {
	clock, testBus := newTestBus()
	master := NewMaster("M0", testBus)
	NewSlave("S0", 0x33, testBus)
	master.Transaction([]Message{{Addr: 0x33, Data: []byte{1, 2, 3}}}, func(Result) {
		t.Fatal("reset must suppress callbacks from discarded work")
	})
	clock.RunUntil(15)
	if testBus.SDA.Level() != Low {
		t.Fatalf("test setup expected START to have pulled SDA LOW")
	}
	testBus.Reset()
	if testBus.SDA.Level() != High || testBus.SCL.Level() != High {
		t.Fatal("reset did not return both lines HIGH")
	}
	if len(testBus.SDA.Owners()) != 1 || testBus.SDA.Owners()[0] != PullUp {
		t.Fatalf("SDA retained owner: %v", testBus.SDA.Owners())
	}

	resultCh := make(chan Result, 1)
	master.Transaction([]Message{{Addr: 0x33, Data: []byte{9}}}, func(result Result) {
		resultCh <- result
	})
	clock.Run()
	if (<-resultCh).Status != StatusCompleted {
		t.Fatal("bus was unusable after reset")
	}
}

func TestSegmentsAttributeEveryLevel(t *testing.T) {
	clock, testBus := newTestBus()
	master := NewMaster("M0", testBus)
	NewSlave("S0", 0x40, testBus)
	runResult(t, clock, master, []Message{{Addr: 0x40, Data: []byte{0}}})

	var ack Segment
	for _, segment := range testBus.Segments() {
		if segment.Signal == SDA && segment.Level == Low {
			for _, owner := range segment.Owners {
				if owner == "S0" {
					ack = segment
				}
			}
		}
	}
	if ack.Owners == nil {
		t.Fatal("SDA ACK segment was not attributed to S0")
	}
	for _, segment := range testBus.Segments() {
		if len(segment.Owners) == 0 {
			t.Fatalf("unowned segment: %s", segment)
		}
	}
}
