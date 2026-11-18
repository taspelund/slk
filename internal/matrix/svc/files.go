package svc

// WP-Media: uploads, downloads, images. This file is the work package's; it owns filesState
// (the Backend's files field) and fillFiles.

// filesState is WP-Media's state on a Backend.
type filesState struct{}

func newFilesState() *filesState { return &filesState{} }

// fillFiles sets WP-Media's ports on p.
func (b *Backend) fillFiles(p *ports) {}
