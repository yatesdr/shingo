package main

import (
	"fmt"
	"io"
	"time"

	"shingocore/domain"
	"shingocore/store"
)

// forecast.go — [S.7] FORECAST: the forecasts' record over the run (D2.3).
// Every record born a forecast, how many still are, and of those that ended:
// opened (a hit, with the median lateness against the first projection),
// expired (a miss), recovered by another hand, withdrawn. store.ForecastScore,
// one read.

const measureForecast = "[S.7] FORECAST"

func readForecast(db *store.DB) (domain.ForecastScore, error) {
	return db.ForecastScore(time.Time{})
}

func printForecast(out io.Writer, s domain.ForecastScore, failed bool) {
	if failed {
		fmt.Fprintf(out, "\n%s  FAILED-TO-RUN\n", measureForecast)
		return
	}
	fmt.Fprintf(out, "\n%s  forecasts born %d, still forecasting %d\n", measureForecast, s.Born, s.Forecasting)
	late := "n/a"
	if s.MedianLateness != nil {
		late = s.MedianLateness.Round(time.Second).String()
	}
	fmt.Fprintf(out, "        opened %d (hits; %d projected, median lateness %s) · expired %d (misses) · "+
		"recovered by another hand %d · withdrawn %d\n",
		s.Opened, s.OpenedProjected, late, s.Expired, s.Recovered, s.Withdrawn)
}
