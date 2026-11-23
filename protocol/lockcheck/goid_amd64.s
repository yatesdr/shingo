//go:build (lockcheck || sim) && gc

#include "textflag.h"

// func gWord(off uintptr) uint64
//
// (TLS) is the running goroutine's g. The assembler rewrites this load for the
// target OS (cmd/internal/obj/x86 progedit: one instruction on linux, through
// runtime.tls_g and GS on windows), as it does the runtime's own get_tls.
TEXT ·gWord(SB), NOSPLIT, $0-16
	MOVQ	(TLS), AX
	MOVQ	off+0(FP), BX
	MOVQ	(AX)(BX*1), AX
	MOVQ	AX, ret+8(FP)
	RET
