## Fleet figures — busy

**Busy means in use and unable to take other work: moving, held or faulted.**
A robot is busy from the order's first acknowledged or in-transit row until it
puts the load down (delivered, or the order's failure), and on an open order
until now. Faulted time counts: the robot is still on its order. The wait
before a robot takes the order is not busy, and nor is the wait for the
station's confirmation after delivery.

Run time (see Mission detail — stages) is not busy time: it measures the work,
so it leaves fault time out.

A robot on two orders at once is busy once, for the union of the two. Every
fleet figure reads this one interval set: per-robot busy and utilization,
fleet utilization and average load, the daily average, the hourly curve and
the peak. `TestOverviewPin_OverlapIsCountedOnce` pins it, including that a
day's average load equals that day's daily average.
