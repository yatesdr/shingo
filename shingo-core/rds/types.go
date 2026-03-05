package rds

// Response is the common RDS API response envelope.
type Response struct {
	Code     int    `json:"code"`
	Msg      string `json:"msg"`
	CreateOn string `json:"create_on"`
}

// OrderState represents RDS order lifecycle states.
type OrderState string

const (
	StateCreated        OrderState = "CREATED"
	StateToBeDispatched OrderState = "TOBEDISPATCHED"
	StateRunning        OrderState = "RUNNING"
	StateFinished       OrderState = "FINISHED"
	StateFailed         OrderState = "FAILED"
	StateStopped        OrderState = "STOPPED"
	StateWaiting        OrderState = "WAITING"
)

func (s OrderState) IsTerminal() bool {
	return s == StateFinished || s == StateFailed || s == StateStopped
}
