package interaction

import "github.com/dryaf/deploy/internal/notification/protocol"

// Spool cursors identify the relay's own durable inbox, independently of a
// notification gateway's recovery epoch. NativeID remains the request identity.
type SpoolCursor struct {
	Identity protocol.Identity `json:"identity"`
	Account  string            `json:"account"`
	Position uint64            `json:"position"`
}
type SpoolEvent struct {
	Position uint64   `json:"position"`
	Key      string   `json:"key"`
	Hash     string   `json:"hash"`
	Inbound  *Inbound `json:"inbound,omitempty"`
}
type SpoolPage struct {
	Events []SpoolEvent `json:"events"`
	Cursor SpoolCursor  `json:"cursor"`
}
