package svc

// WP-Profile: profiles and presence. This file is the work package's; it owns profileState
// (the Backend's profile field) and fillProfile.

// profileState is WP-Profile's state on a Backend.
type profileState struct{}

func newProfileState() *profileState { return &profileState{} }

// fillProfile sets WP-Profile's ports on p.
func (b *Backend) fillProfile(p *ports) {}
