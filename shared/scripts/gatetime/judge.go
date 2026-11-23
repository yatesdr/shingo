package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// sample is one sampler pass: cumulative readings, so any two samples give
// the averages between them.
type sample struct {
	at    time.Time // wall clock, monotonic reading stripped
	phase string    // before, during, after
	host  time.Duration
	job   time.Duration
	vm    time.Duration
	ac    int // 1 on, 0 off, -1 unknown
	err   string
	top   string // the largest foreign processes since the last pass, for a reader
}

// The rules' constants.
const (
	// foreignWindow and foreignCores: foreign load voids a run when it
	// averages more than one core over any 30 s.
	foreignWindow = 30 * time.Second
	foreignCores  = 1.0
	// vmBeforeCores: the utility VM busier than this before the command
	// starts is load nobody in this run made (see run.go's hole).
	vmBeforeCores = 1.0
)

// judgement is what the rules say about a run's samples so far.
type judgement struct {
	reasons      []string
	foreignMax30 float64 // cores, the busiest 30 s window (the whole span when shorter)
	gapMax       time.Duration
	acState      string // on, off, unknown
	standby      int
}

func (j judgement) verdict() string {
	if len(j.reasons) == 0 {
		return "clean"
	}
	return "VOID: " + strings.Join(j.reasons, " ")
}

// foreign is the cumulative CPU nobody in the run accounts for.
func (s sample) foreign() time.Duration { return s.host - s.job - s.vm }

// judge applies every rule. maxGap is the longest wall-clock gap between
// two passes that is not a standby or a frozen sampler.
func judge(ss []sample, evs []standbyEvent, maxGap time.Duration, plants, limits []string) judgement {
	var j judgement
	add := func(r string) { j.reasons = append(j.reasons, r) }
	if len(ss) < 2 {
		add("no-samples")
	}
	errs, on, off := 0, 0, 0
	for i, s := range ss {
		if s.err != "" {
			errs++
		}
		switch s.ac {
		case 1:
			on++
		case 0:
			off++
		}
		if i > 0 {
			j.gapMax = max(j.gapMax, s.at.Sub(ss[i-1].at))
		}
	}
	if errs > 0 {
		add(fmt.Sprintf("sampler-error(%d passes)", errs))
	}
	switch {
	case off > 0:
		j.acState = "off"
		add(fmt.Sprintf("not-on-AC(%d of %d samples)", off, len(ss)))
	case on == len(ss) && on > 0:
		j.acState = "on"
	default:
		j.acState = "unknown"
		add("AC-unknown")
	}
	j.standby = len(evs)
	if len(evs) > 0 {
		add(fmt.Sprintf("standby(%s)", eventList(evs)))
	}
	if j.gapMax > maxGap {
		add(fmt.Sprintf("sampler-gap(%.0fs)", j.gapMax.Seconds()))
	}
	j.foreignMax30 = foreignMax(ss)
	if j.foreignMax30 > foreignCores {
		add(fmt.Sprintf("foreign-load(%.2f cores over 30 s)", j.foreignMax30))
	}
	if c, ok := vmBefore(ss); ok && c > vmBeforeCores {
		add(fmt.Sprintf("vm-busy-before(%.2f cores)", c))
	}
	if len(plants) > 0 {
		add("planted(" + strings.Join(plants, ",") + ")")
	}
	for _, l := range limits {
		add(l)
	}
	return j
}

// foreignMax is the busiest foreign load, in cores, over any window of at
// least foreignWindow ending at a pass; over the whole span when the samples
// cover less than that.
func foreignMax(ss []sample) float64 {
	if len(ss) < 2 {
		return 0
	}
	cores := func(a, b sample) float64 {
		d := b.at.Sub(a.at).Seconds()
		if d <= 0 {
			return 0
		}
		return (b.foreign() - a.foreign()).Seconds() / d
	}
	first, last := ss[0], ss[len(ss)-1]
	if last.at.Sub(first.at) < foreignWindow {
		return cores(first, last)
	}
	best, j := 0.0, 0
	for i := 1; i < len(ss); i++ {
		// the latest start that still leaves a full window
		for j+1 < i && ss[i].at.Sub(ss[j+1].at) >= foreignWindow {
			j++
		}
		if ss[i].at.Sub(ss[j].at) >= foreignWindow {
			best = max(best, cores(ss[j], ss[i]))
		}
	}
	return best
}

// vmBefore is the utility VM's load, in cores, between the passes before the
// command started.
func vmBefore(ss []sample) (float64, bool) {
	var b []sample
	for _, s := range ss {
		if s.phase == "before" {
			b = append(b, s)
		}
	}
	if len(b) < 2 {
		return 0, false
	}
	d := b[len(b)-1].at.Sub(b[0].at).Seconds()
	if d <= 0 {
		return 0, false
	}
	return (b[len(b)-1].vm - b[0].vm).Seconds() / d, true
}

func eventList(evs []standbyEvent) string {
	s := append([]standbyEvent(nil), evs...)
	sort.Slice(s, func(a, b int) bool { return s[a].at.Before(s[b].at) })
	var out []string
	for _, e := range s {
		out = append(out, e.id+"@"+e.at.UTC().Format("15:04:05"))
	}
	return strings.Join(out, ",")
}
