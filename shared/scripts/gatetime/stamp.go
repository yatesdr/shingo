package main

// `gatetime stamp -since UNIX` prints the load stamps scripts/gate.sh puts on
// its steps.log `total` line, space-separated key=value words:
//
//	ac=on|off|unknown  every sample of the timed run on mains power (under
//	                   `gatetime run`), or one reading now (not under it)
//	samples=N          how many readings ac stands on
//	standby=N          Kernel-Power 506/507 events since -since; "unread"
//	                   when the log could not be read
//	load=clean|void|unmeasured   the timed run's judgement so far (foreign
//	                   load, gaps, AC); unmeasured outside `gatetime run`
//	foreign30=X        the busiest 30 s of foreign load so far, in cores
//	planted=a,b        only when GATETIME_PLANT is set (test only)
//
// Under `gatetime run` it reads GATETIME_RUN_DIR/live.tsv, which the sampler
// rewrites every pass. Its reader is `gatetime compare`, which refuses a run
// whose total line stamps ac other than on, standby other than 0, or load
// other than clean.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func stampMain(args []string) int {
	fs := flag.NewFlagSet("stamp", flag.ContinueOnError)
	since := fs.Float64("since", 0, "the gate's start, Unix seconds")
	if err := fs.Parse(args); err != nil || *since <= 0 {
		fmt.Fprintln(os.Stderr, "usage: gatetime stamp -since UNIX")
		return 2
	}
	m, plants := plantedMachine()
	from := time.Unix(0, int64(*since*1e9))
	fmt.Println(stamps(m, plants, from, time.Now(), os.Getenv("GATETIME_RUN_DIR")))
	return 0
}

func stamps(m machine, plants []string, from, to time.Time, runDir string) string {
	var out []string
	live := resultLine{}
	if runDir != "" {
		if data, err := os.ReadFile(filepath.Join(runDir, "live.tsv")); err == nil {
			live = parseResultLine(strings.SplitN(string(data), "\n", 2)[0])
		}
	}
	if live["samples"] != "" {
		out = append(out, "ac="+live["ac"], "samples="+live["samples"])
	} else {
		a, err := m.ac()
		state := map[int]string{1: "on", 0: "off"}[a]
		if err != nil || state == "" {
			state = "unknown"
		}
		out = append(out, "ac="+state, "samples=1")
	}
	if evs, err := m.standby(from, to); err != nil {
		out = append(out, "standby=unread")
	} else {
		out = append(out, "standby="+strconv.Itoa(len(evs)))
	}
	switch v := live["verdict"]; {
	case v == "":
		out = append(out, "load=unmeasured")
	case v == "clean":
		out = append(out, "load=clean", "foreign30="+live["foreign_max30"])
	default:
		out = append(out, "load=void", "foreign30="+live["foreign_max30"])
	}
	if len(plants) > 0 {
		out = append(out, "planted="+strings.Join(plants, ","))
	}
	return strings.Join(out, " ")
}
