package svc

// WP-Write: send, edit, delete, reactions, receipts, joins, typing. This file is the work package's; it owns writeState
// (the Backend's write field) and fillWrite.

// writeState is WP-Write's state on a Backend.
type writeState struct{}

func newWriteState() *writeState { return &writeState{} }

// fillWrite sets WP-Write's ports on p.
func (b *Backend) fillWrite(p *ports) {}
