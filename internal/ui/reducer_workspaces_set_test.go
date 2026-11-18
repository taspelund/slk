package ui

import (
	"testing"

	"github.com/gammons/slk/internal/ui/workspace"
)

// TestWorkspacesSetReplacesTheRail: a backend's new workspace list
// replaces the rail's items, and the active workspace stays selected
// though its position moved.
func TestWorkspacesSetReplacesTheRail(t *testing.T) {
	a := newTestApp(t,
		withWorkspaces(workspace.WorkspaceItem{ID: "T1", Name: "One", Initials: "O"}),
		withActiveTeam("T1"),
	)
	a.Update(WorkspacesSetMsg{Items: []workspace.WorkspaceItem{
		{ID: "T0", Name: "Zero", Initials: "Z"},
		{ID: "T1", Name: "One", Initials: "O"},
	}})
	items := a.workspaceRail.Items()
	if len(items) != 2 || items[0].ID != "T0" || items[1].ID != "T1" {
		t.Fatalf("rail = %+v", items)
	}
	if got := a.workspaceRail.SelectedID(); got != "T1" {
		t.Errorf("selected = %q, want the active T1", got)
	}
}
