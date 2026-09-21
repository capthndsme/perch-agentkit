package link

import (
	"context"
	"log/slog"
	"time"
)

// Push interval bounds for IntervalFromSeconds.
const (
	MinPushInterval = time.Second
	MaxPushInterval = time.Hour
)

// Schedule is the push cadence the controller sets with agent.configure.
// Interval 0 pauses pushing.
type Schedule struct {
	Interval time.Duration
}

// IntervalFromSeconds turns agent.configure's seconds into an interval: 0 or
// less pauses, anything else is clamped to [MinPushInterval, MaxPushInterval].
func IntervalFromSeconds(secs float64) time.Duration {
	if secs <= 0 {
		return 0
	}
	d := time.Duration(secs * float64(time.Second))
	if d < MinPushInterval {
		d = MinPushInterval
	}
	if d > MaxPushInterval {
		d = MaxPushInterval
	}
	return d
}

// Offer puts s on ch, replacing whatever schedule is still waiting there:
// only the newest one matters. It never blocks.
func Offer(ch chan Schedule, s Schedule) {
	for {
		select {
		case ch <- s:
			return
		default:
			select {
			case <-ch:
			default:
			}
		}
	}
}

// PushOptions configure RunPusher.
type PushOptions struct {
	// Configs delivers the controller's schedules (see Offer).
	Configs <-chan Schedule
	// Fallback is used when no schedule arrives within FallbackWait (a
	// controller too old to send one). nil waits for a schedule forever.
	Fallback     *Schedule
	FallbackWait time.Duration
	// MinGap is the shortest wait between two pushes (default MinPushInterval).
	MinGap time.Duration
	// Push sends one push; seq counts from 1 within the session.
	Push func(ctx context.Context, seq uint64)
	Log  *slog.Logger
}

// RunPusher pushes on the controller's schedule until ctx is done: the first
// push right after the first schedule, then one per interval measured from
// the start of the previous push, so a slow collection does not drift the
// cadence. A later schedule keeps the cadence with the new interval;
// interval 0 pauses and the next non-zero schedule resumes it.
func RunPusher(ctx context.Context, o PushOptions) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	minGap := o.MinGap
	if minGap <= 0 {
		minGap = MinPushInterval
	}
	var (
		cfg        Schedule
		configured bool
		last       time.Time
		seq        uint64
		timer      *time.Timer
		timerC     <-chan time.Time
		fallbackC  <-chan time.Time
	)
	if o.Fallback != nil {
		ft := time.NewTimer(o.FallbackWait)
		defer ft.Stop()
		fallbackC = ft.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	arm := func() {
		if cfg.Interval <= 0 {
			if timer != nil {
				timer.Stop()
			}
			timerC = nil
			return
		}
		d := time.Until(last.Add(cfg.Interval))
		if d < minGap {
			d = minGap
		}
		if timer == nil {
			timer = time.NewTimer(d)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(d)
		}
		timerC = timer.C
	}
	push := func() {
		last = time.Now()
		seq++
		o.Push(ctx, seq)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-o.Configs:
			fallbackC = nil
			first := !configured
			configured, cfg = true, c
			if cfg.Interval > 0 && (first || last.IsZero()) {
				push()
			}
			arm()
		case <-fallbackC:
			fallbackC = nil
			if configured {
				continue
			}
			configured, cfg = true, *o.Fallback
			log.Warn("the controller sent no schedule; pushing with the defaults", "interval", cfg.Interval.String())
			if cfg.Interval > 0 {
				push()
			}
			arm()
		case <-timerC:
			push()
			arm()
		}
	}
}
