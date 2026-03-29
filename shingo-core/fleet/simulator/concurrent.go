package simulator

import (
	"sync"
)

// ParallelGroup runs n functions concurrently with a channel barrier.
// All goroutines block until every one is ready, then all start simultaneously.
func ParallelGroup(n int, fn func(i int)) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			fn(idx)
		}(i)
	}
	close(start)
	wg.Wait()
}
