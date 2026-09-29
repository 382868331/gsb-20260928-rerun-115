package bus

import "sort"

// Unit is the virtual time unit. Nothing in this package depends on a real
// wall clock or on any hardware interface.
type Unit int64

type timedFunc struct {
	at  Unit
	seq int64
	fn  func(now Unit)
}

// Clock is a deterministic, single-threaded virtual clock. Callbacks run in
// time order; callbacks at the same time run in posting order.
type Clock struct {
	Now   Unit
	seq   int64
	queue []timedFunc
}

func (c *Clock) Post(delay Unit, fn func(now Unit)) {
	if delay < 0 {
		delay = 0
	}
	c.seq++
	c.queue = append(c.queue, timedFunc{at: c.Now + delay, seq: c.seq, fn: fn})
}

func (c *Clock) pop() (timedFunc, bool) {
	if len(c.queue) == 0 {
		return timedFunc{}, false
	}
	sort.SliceStable(c.queue, func(i, j int) bool {
		if c.queue[i].at != c.queue[j].at {
			return c.queue[i].at < c.queue[j].at
		}
		return c.queue[i].seq < c.queue[j].seq
	})
	item := c.queue[0]
	c.queue = c.queue[1:]
	return item, true
}

// Run advances until there is no more work. New callbacks may be posted by a
// running callback.
func (c *Clock) Run() {
	for {
		item, ok := c.pop()
		if !ok {
			return
		}
		c.Now = item.at
		item.fn(c.Now)
	}
}

// RunUntil drains callbacks due no later than deadline. This allows a test to
// interrupt an in-progress transaction at an exact virtual instant.
func (c *Clock) RunUntil(deadline Unit) {
	for {
		item, ok := c.pop()
		if !ok || item.at > deadline {
			c.Now = deadline
			return
		}
		c.Now = item.at
		item.fn(c.Now)
	}
}
