package rds

// --- Mutex types ---

type MutexGroupRequest struct {
	ID         string   `json:"id"`
	BlockGroup []string `json:"blockGroup"`
}

type MutexGroupResult struct {
	Name       string `json:"name"`
	IsOccupied bool   `json:"isOccupied"`
	Occupier   string `json:"occupier"`
}

// MutexGroupStatus is an alias for MutexGroupResult (same shape, used by GetMutexGroupStatus).
type MutexGroupStatus = MutexGroupResult
