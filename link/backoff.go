package link

import (
	"math/rand"
	"time"
)

// Backoff is the reconnect delay: Min, doubling up to Max, with ±10 %
// jitter so a fleet of agents does not reconnect in lockstep.
type Backoff struct {
	Min, Max time.Duration
	cur      time.Duration
}

// Next returns the next delay.
func (b *Backoff) Next() time.Duration {
	if b.cur == 0 {
		b.cur = b.Min
	} else {
		b.cur *= 2
		if b.cur > b.Max {
			b.cur = b.Max
		}
	}
	jitter := time.Duration(rand.Int63n(int64(b.cur)/5+1)) - b.cur/10
	return b.cur + jitter
}

// Reset starts the ladder over (after a session that lasted).
func (b *Backoff) Reset() { b.cur = 0 }
