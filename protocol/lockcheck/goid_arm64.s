//go:build (lockcheck || sim) && gc

#include "textflag.h"

// func gWord(off uintptr) uint64
//
// g (R28) holds the running goroutine's g on arm64.
TEXT ·gWord(SB), NOSPLIT, $0-16
	MOVD	off+0(FP), R0
	MOVD	g, R1
	ADD	R0, R1
	MOVD	(R1), R0
	MOVD	R0, ret+8(FP)
	RET
