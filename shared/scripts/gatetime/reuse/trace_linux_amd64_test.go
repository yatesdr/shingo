//go:build linux && amd64

package reuse

import "testing"

// TestFilterProgram_EveryJumpLandsOnAReturn pins the shim's BPF program: a
// jump that lands off the end, or on anything but a return, is a filter the
// kernel refuses (the run falls back to untraced) or one that lets a traced
// syscall through.
func TestFilterProgram_EveryJumpLandsOnAReturn(t *testing.T) {
	const jeq, jge, ret = 0x15, 0x35, 0x06
	p := filterProgram()
	traced := map[uint32]bool{}
	for i, ins := range p {
		if ins.code != jeq && ins.code != jge {
			continue
		}
		if i == 1 {
			continue // the arch check: x86_64 jumps on to the syscall-number load
		}
		if ins.jf != 0 {
			t.Errorf("instruction %d falls through by %d, want 0", i, ins.jf)
		}
		j := i + 1 + int(ins.jt)
		if j >= len(p) || p[j].code != ret {
			t.Fatalf("instruction %d jumps to %d, which is not a return", i, j)
		}
		if i >= 6 {
			traced[ins.k] = true
			if want := map[bool]uint32{true: seccompRetTrace | traceDataBlind, false: seccompRetTrace | traceDataPath}; p[j].k != want[blindNr(ins.k)] {
				t.Errorf("syscall %d returns %#x", ins.k, p[j].k)
			}
		}
	}
	for _, s := range pathSyscalls {
		if !traced[uint32(s.nr)] {
			t.Errorf("syscall %d is in pathSyscalls but not in the filter", s.nr)
		}
	}
	if last := p[len(p)-1]; last.code != ret || last.k != seccompRetErrno|38 /* ENOSYS */ {
		t.Errorf("the last instruction is %+v, want the io_uring ENOSYS return", last)
	}
}

func blindNr(nr uint32) bool {
	for _, s := range pathSyscalls {
		if uint32(s.nr) == nr {
			return s.op == ""
		}
	}
	return false
}
