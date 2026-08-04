package main

import (
	"fmt"
	"os"

	"shingocore/notify"
)

func main() {
	from := "shingo@company.com"
	to := []string{"maintenance@company.com"}

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  EVENT 1: ORDER FAULTED                                   ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Print(notify.FormatMessage(from, to,
		notify.FaultSubject(),
		notify.FaultAlert(3548, "f0108578-604d-4069-951e-01581b2ca64a", "plant-a.line-1", "fleet state: FAILED", "AMR-11"),
	))
	fmt.Println("──────────────────────────────────────────────────────────────────")

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  EVENT 2: ORDER FAILED                                     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Print(notify.FormatMessage(from, to,
		notify.FailSubject(),
		notify.FailAlert(3548, "f0108578-604d-4069-951e-01581b2ca64a", "plant-a.line-1", "fleet_failed", "grace period expired without fleet recovery", "AMR-11"),
	))
	fmt.Println("──────────────────────────────────────────────────────────────────")

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  EVENT 3: GRACE EXPIRED                                     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Print(notify.FormatMessage(from, to,
		notify.GraceExpiredSubject(),
		notify.GraceExpiredAlert(3549, "sg-3549-81463c07", "AMR-11"),
	))
	fmt.Println("──────────────────────────────────────────────────────────────────")

	fmt.Println()
	os.Exit(0)
}
