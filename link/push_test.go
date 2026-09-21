package link

import (
	"context"
	"sync"
	"testing"
	"time"
)

type pushLog struct {
	mu  sync.Mutex
	at  []time.Time
	seq []uint64
	ch  chan uint64
}

func (p *pushLog) push(_ context.Context, seq uint64) {
	p.mu.Lock()
	p.at = append(p.at, time.Now())
	p.seq = append(p.seq, seq)
	p.mu.Unlock()
	p.ch <- seq
}

func expectPush(t *testing.T, p *pushLog, within time.Duration) uint64 {
	t.Helper()
	select {
	case s := <-p.ch:
		return s
	case <-time.After(within):
		t.Fatal("no push")
		return 0
	}
}

func expectQuiet(t *testing.T, p *pushLog, d time.Duration) {
	t.Helper()
	select {
	case s := <-p.ch:
		t.Fatalf("unexpected push %d", s)
	case <-time.After(d):
	}
}

func TestPusherFollowsTheSchedule(t *testing.T) {
	p := &pushLog{ch: make(chan uint64, 16)}
	configs := make(chan Schedule, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunPusher(ctx, PushOptions{Configs: configs, Push: p.push, MinGap: time.Millisecond, Log: quiet})

	expectQuiet(t, p, 100*time.Millisecond) // nothing before the first schedule
	Offer(configs, Schedule{Interval: 80 * time.Millisecond})
	if s := expectPush(t, p, time.Second); s != 1 {
		t.Fatalf("first seq %d", s)
	}
	if s := expectPush(t, p, time.Second); s != 2 {
		t.Fatalf("second seq %d", s)
	}
	p.mu.Lock()
	gap := p.at[1].Sub(p.at[0])
	p.mu.Unlock()
	if gap < 60*time.Millisecond || gap > 400*time.Millisecond {
		t.Fatalf("gap %v", gap)
	}

	Offer(configs, Schedule{}) // pause
	time.Sleep(20 * time.Millisecond)
	for len(p.ch) > 0 {
		<-p.ch
	}
	expectQuiet(t, p, 250*time.Millisecond)
	Offer(configs, Schedule{Interval: 50 * time.Millisecond}) // resume on the old cadence
	if s := expectPush(t, p, time.Second); s < 3 {
		t.Fatalf("resumed seq %d", s)
	}
}

func TestPusherFallback(t *testing.T) {
	p := &pushLog{ch: make(chan uint64, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunPusher(ctx, PushOptions{
		Configs: make(chan Schedule), Push: p.push, MinGap: time.Millisecond, Log: quiet,
		Fallback: &Schedule{Interval: 50 * time.Millisecond}, FallbackWait: 30 * time.Millisecond,
	})
	expectPush(t, p, time.Second)
	expectPush(t, p, time.Second)
}

func TestIntervalFromSecondsAndOffer(t *testing.T) {
	for in, want := range map[float64]time.Duration{0: 0, -1: 0, 0.2: time.Second, 5: 5 * time.Second, 99999: time.Hour} {
		if got := IntervalFromSeconds(in); got != want {
			t.Errorf("%v -> %v", in, got)
		}
	}
	ch := make(chan Schedule, 1)
	Offer(ch, Schedule{Interval: time.Second})
	Offer(ch, Schedule{Interval: 2 * time.Second})
	if got := <-ch; got.Interval != 2*time.Second {
		t.Fatalf("offer kept %v", got)
	}
}
