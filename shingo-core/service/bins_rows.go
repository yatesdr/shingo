package service

import (
	"log"
	"sync/atomic"

	"shingo/protocol"
)

// binsRows reads Core's current bins rows for node names (the messaging
// layer's CoreDataService.NodeBinsRows), wired once through
// Engine.SetBinsRowsFunc. Atomic because the composition root sets it after
// HTTP handlers may already be reading.
type binsRowsHook = atomic.Pointer[func(names []string) ([]protocol.NodeBinsRow, error)]

// SetBinsRowsFunc installs the read behind BinsRows. Set once, through
// Engine.SetBinsRowsFunc.
func (s *NodeService) SetBinsRowsFunc(fn func(names []string) ([]protocol.NodeBinsRow, error)) {
	s.binsRows.Store(&fn)
}

// BinsRows is Core's current bins rows for the nodes a write door touched, for
// its refusal: the Edge writes them into its copy before it shows the
// sentence. nil when nothing is wired or the read fails (logged): a refusal
// without rows is the refusal it always was.
func (s *NodeService) BinsRows(names ...string) []protocol.NodeBinsRow {
	fn := s.binsRows.Load()
	if fn == nil || *fn == nil || len(names) == 0 {
		return nil
	}
	rows, err := (*fn)(names)
	if err != nil {
		log.Printf("bins rows for %v: %v", names, err)
		return nil
	}
	return rows
}
