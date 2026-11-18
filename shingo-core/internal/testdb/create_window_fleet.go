package testdb

import (
	"errors"
	"runtime"
	"sync"

	"shingocore/fleet"
)

// CreateWindowFleet is a fleet a test can stand inside a create with, and whose
// answer the test chooses. Every CreateOrder, refused or accepted, records the
// id asked and runs Inside before it answers; Inside's return is the answer:
//
//   - nil: the fleet takes the job (the MockBackend records it, as every
//     accepted create);
//   - TookItAndDied: the fleet takes the job, and the goroutine that asked stops
//     before Core hears the answer (runtime.Goexit), as a Core killed after the
//     fleet accepted;
//   - DiedBeforeAnswer: the goroutine that asked stops with the fleet holding
//     nothing, as a Core killed before the fleet answered;
//   - TookItAndLostTheAnswer: the fleet takes the job and Core's call returns
//     TookItAndLostTheAnswer, a client timeout, as a create whose answer was lost
//     on the way back;
//   - any other error: the fleet refuses the create with it.
//
// WHY A FLEET OF ITS OWN. MockBackend's refusing arm returns before its own
// SetOnCreate hook, so a refused create's window could not be observed, and a
// refusal pin could only assert what was left after it. The window is the
// stretch between the hand's commit and the fleet's answer, where every reader
// of "Core has claimed it" and of "the fleet has it" is asked its question; it
// is also where a Core dies with an ask in flight. A test that dies inside a
// create runs the dispatch on a goroutine of its own and waits for it: Goexit
// runs the goroutine's deferred calls (its transaction's rollback, its scan
// lock), and nothing after the create.
//
// It embeds the tracking mock, so an engine takes it as its fleet.
type CreateWindowFleet struct {
	*MockTrackingBackend
	// Inside runs inside every create; n is the create's 1-based count over the
	// fleet's life. Nil answers every create with nil (the fleet takes it).
	Inside func(n int, req fleet.CreateOrderRequest) error

	mu    sync.Mutex
	asked []string
}

// TookItAndDied and DiedBeforeAnswer are Inside's two deaths, and
// TookItAndLostTheAnswer its lost answer (see CreateWindowFleet). The last is
// worded as the HTTP client's own timeout, which is what Core is handed.
var (
	TookItAndDied          = errors.New("testdb: the fleet took the job and Core died before hearing it")
	DiedBeforeAnswer       = errors.New("testdb: Core died before the fleet answered")
	TookItAndLostTheAnswer = errors.New(`Post "http://rds/setOrder": context deadline exceeded ` +
		`(Client.Timeout exceeded while awaiting headers)`)
)

// NewCreateWindowFleet returns a CreateWindowFleet that takes every create
// until its Inside says otherwise.
func NewCreateWindowFleet() *CreateWindowFleet {
	return &CreateWindowFleet{MockTrackingBackend: NewTrackingBackend()}
}

// CreateOrder records the ask, runs Inside, and answers with what it returned.
func (f *CreateWindowFleet) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	f.mu.Lock()
	f.asked = append(f.asked, req.OrderID)
	n := len(f.asked)
	inside := f.Inside
	f.mu.Unlock()

	var answer error
	if inside != nil {
		answer = inside(n, req)
	}
	switch {
	case errors.Is(answer, DiedBeforeAnswer):
		runtime.Goexit()
	case errors.Is(answer, TookItAndDied):
		f.take(req)
		runtime.Goexit()
	case errors.Is(answer, TookItAndLostTheAnswer):
		f.take(req)
		return fleet.TransportOrderResult{}, TookItAndLostTheAnswer
	case answer != nil:
		return fleet.TransportOrderResult{}, answer
	}
	return f.MockTrackingBackend.CreateOrder(req)
}

// take records req as a job the fleet holds, whatever Core then hears.
func (f *CreateWindowFleet) take(req fleet.CreateOrderRequest) {
	if _, err := f.MockTrackingBackend.CreateOrder(req); err != nil {
		panic("testdb: CreateWindowFleet could not record the job it took: " + err.Error())
	}
}

// Asked is every vendor order id the fleet was asked to create, in order,
// whatever it answered. CreateRequests (the embedded mock's) holds the ones it
// took.
func (f *CreateWindowFleet) Asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}
