// The wire contract and the infrastructure both servers run: the JSON envelope, message types and payloads, statuses, TTL expiry, and the outbox, clock, debuglog, auth and migrate packages.
module shingo/protocol

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	golang.org/x/crypto v0.48.0
	golang.org/x/mod v0.27.0
)
