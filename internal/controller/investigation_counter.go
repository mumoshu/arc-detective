package controller

import "sync"

const DefaultMaxInvestigations = 100

// InvestigationCounter tracks the number of Investigation CRs in the cluster
// in-memory, avoiding repeated List calls on every reconcile. It is initialized
// once at startup and updated as investigations are created or deleted.
type InvestigationCounter struct {
	mu    sync.Mutex
	count int
	max   int
}

// NewInvestigationCounter creates a counter with the given max. Panics if max is 0.
func NewInvestigationCounter(max int) *InvestigationCounter {
	if max == 0 {
		panic("maxInvestigations must be > 0")
	}
	return &InvestigationCounter{max: max}
}

// Init sets the current count, typically from an initial List at startup.
func (c *InvestigationCounter) Init(count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count = count
}

// TryIncrement atomically checks whether creating another investigation is
// allowed. If the count is below max it increments and returns true. Otherwise
// it returns false without modifying the count.
func (c *InvestigationCounter) TryIncrement() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count >= c.max {
		return false
	}
	c.count++
	return true
}

// Decrement reduces the count by n (floored at 0).
func (c *InvestigationCounter) Decrement(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count -= n
	if c.count < 0 {
		c.count = 0
	}
}

// Count returns the current investigation count.
func (c *InvestigationCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}
