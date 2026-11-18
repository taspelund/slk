// internal/ui/verify.go
//
// Interactive device verification (on Matrix: comparing emoji with another
// of the account's devices). The backend reports each step with a
// Verify*Msg; the App asks the user in the confirm prompt and sends the
// answers back through core.VerificationService. A dialog waits, like an
// action dialog, while the user is busy (another modal, typing, a ':'
// command), and opens at the same re-check points (tryOpenActionPrompt),
// ahead of any action dialog: a verification times out.
package ui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/bubbles/confirmprompt"
	"github.com/gammons/slk/internal/core"
)

// VerifyRequestMsg is another of the account's devices asking to verify
// this one. ID identifies the verification in later messages and in the
// answers; Device names the other device for the user.
type VerifyRequestMsg struct{ ID, Device string }

// VerifyEmojiMsg is the emoji of verification ID, to compare with the ones
// Device shows.
type VerifyEmojiMsg struct {
	ID, Device string
	Emoji      []VerifyEmoji
}

// VerifyEmoji is one emoji to compare: its glyph and its name.
type VerifyEmoji struct{ Glyph, Name string }

// VerifyEndMsg says verification ID needs nothing more from the user
// (verified, cancelled, timed out): its dialog closes, shown or waiting,
// and Text, when set, is toasted.
type VerifyEndMsg struct{ ID, Text string }

// verifyEndToast is how long a VerifyEndMsg's toast stays.
const verifyEndToast = 6 * time.Second

// verifyState is the App's side of verification.
type verifyState struct {
	svc core.VerificationService
	// queue holds the dialogs waiting to open, oldest first, at most one
	// per verification: a later step replaces its earlier one.
	queue []verifyPrompt
	// shown is the verification whose dialog is on screen, "" for none.
	// clearShownPrompt resets it when the prompt closes.
	shown string
}

// verifyPrompt is a waiting dialog. wait is the toast saying it waits;
// toasted records that it was shown.
type verifyPrompt struct {
	id      string
	choice  confirmprompt.Choice
	wait    string
	toasted bool
}

// SetVerificationService wires device verification. nil (Slack) turns
// :verify into a refusal.
func (a *App) SetVerificationService(s core.VerificationService) {
	a.verify.svc = s
}

var reduceVerify reducerFunc = func(a *App, msg tea.Msg) (tea.Cmd, bool) {
	switch m := msg.(type) {
	case VerifyRequestMsg:
		return a.queueVerifyPrompt(verifyPrompt{
			id:     m.ID,
			choice: a.verifyRequestChoice(m),
			wait:   "Verification request from " + m.Device + ": :verify to answer",
		}), true
	case VerifyEmojiMsg:
		return a.queueVerifyPrompt(verifyPrompt{
			id:     m.ID,
			choice: a.verifyEmojiChoice(m),
			wait:   "Emoji to compare with " + m.Device + ": :verify to answer",
		}), true
	case VerifyEndMsg:
		return a.endVerify(m), true
	}
	return nil, false
}

// queueVerifyPrompt queues p, replacing a waiting dialog of the same
// verification, and opens it if the user is not busy. A dialog of the
// same verification on screen is replaced in place.
func (a *App) queueVerifyPrompt(p verifyPrompt) tea.Cmd {
	a.dropVerifyPrompts(p.id)
	if a.verify.shown == p.id && a.mode == ModeConfirm && a.confirmPrompt.IsVisible() {
		a.prepareConfirmPrompt()
		a.confirmPrompt.OpenChoice(p.choice)
		return nil
	}
	a.verify.queue = append(a.verify.queue, p)
	return a.tryOpenActionPrompt()
}

func (a *App) dropVerifyPrompts(id string) {
	kept := a.verify.queue[:0]
	for _, p := range a.verify.queue {
		if p.id != id {
			kept = append(kept, p)
		}
	}
	a.verify.queue = kept
}

// tryOpenVerifyPrompt opens the oldest waiting verification dialog unless
// the user is busy, in which case it toasts once that it waits. waiting
// reports that there was one, so tryOpenActionPrompt leaves its own queue
// for later.
func (a *App) tryOpenVerifyPrompt() (cmd tea.Cmd, waiting bool) {
	if len(a.verify.queue) == 0 {
		return nil, false
	}
	p := &a.verify.queue[0]
	if a.promptMustWait() {
		if p.toasted {
			return nil, true
		}
		p.toasted = true
		return toastWithClear(a, p.wait, verifyEndToast), true
	}
	a.verify.queue = a.verify.queue[1:]
	a.openQueuedChoice(p.choice)
	a.verify.shown = p.id
	return nil, true
}

// endVerify closes verification m.ID's dialog and toasts m.Text.
func (a *App) endVerify(m VerifyEndMsg) tea.Cmd {
	a.dropVerifyPrompts(m.ID)
	var cmds []tea.Cmd
	if a.verify.shown == m.ID && a.mode == ModeConfirm && a.confirmPrompt.IsVisible() {
		a.confirmPrompt.Close()
		cmds = append(cmds, a.afterConfirmPrompt(nil))
	}
	if m.Text != "" {
		cmds = append(cmds, toastWithClear(a, m.Text, verifyEndToast))
	}
	return tea.Batch(cmds...)
}

// verifyRequestChoice is the dialog for another device's request.
func (a *App) verifyRequestChoice(m VerifyRequestMsg) confirmprompt.Choice {
	svc, id := a.verify.svc, m.ID
	return confirmprompt.Choice{
		Title:   "Verify this device?",
		Body:    m.Device + " asks to verify this device. Accept, then compare the emoji both devices show.",
		Default: -1,
		Buttons: []confirmprompt.Button{
			{Label: "Accept", Keys: key.NewBinding(key.WithKeys("y")), Action: verifyAnswer(svc, func(s core.VerificationService) core.Msg { return s.Accept(id) })},
			{Label: "Decline", Keys: key.NewBinding(key.WithKeys("n", "esc")), Action: verifyAnswer(svc, func(s core.VerificationService) core.Msg { return s.Cancel(id, false) })},
		},
	}
}

// verifyEmojiChoice is the dialog comparing the emoji, one per line.
func (a *App) verifyEmojiChoice(m VerifyEmojiMsg) confirmprompt.Choice {
	svc, id := a.verify.svc, m.ID
	lines := make([]string, len(m.Emoji))
	for i, e := range m.Emoji {
		lines[i] = e.Glyph + "  " + e.Name
	}
	return confirmprompt.Choice{
		Title:   fmt.Sprintf("Does %s show these emoji, in this order?", m.Device),
		Body:    strings.Join(lines, "\n"),
		Default: -1,
		Buttons: []confirmprompt.Button{
			{Label: "They match", Keys: key.NewBinding(key.WithKeys("y")), Action: verifyAnswer(svc, func(s core.VerificationService) core.Msg { return s.Confirm(id) })},
			{Label: "They don't match", Keys: key.NewBinding(key.WithKeys("n")), Action: verifyAnswer(svc, func(s core.VerificationService) core.Msg { return s.Cancel(id, true) })},
			{Label: "Cancel", Keys: key.NewBinding(key.WithKeys("c", "esc")), Action: verifyAnswer(svc, func(s core.VerificationService) core.Msg { return s.Cancel(id, false) })},
		},
	}
}

// verifyAnswer is a dialog button's action: answer through svc, off the
// UI goroutine.
func verifyAnswer(svc core.VerificationService, call func(core.VerificationService) core.Msg) func() tea.Msg {
	return func() tea.Msg {
		if svc == nil {
			return nil
		}
		return call(svc)
	}
}

// cmdVerify is :verify: it opens a waiting verification dialog, or else
// asks the account's other devices to verify this one.
func cmdVerify(a *App, _ commandArgs) tea.Cmd {
	svc := a.verify.svc
	if svc == nil {
		return toastWithClear(a, "Device verification is for Matrix accounts", 3*time.Second)
	}
	if len(a.verify.queue) > 0 {
		cmd, _ := a.tryOpenVerifyPrompt()
		return cmd
	}
	return tea.Batch(
		toastWithClear(a, "Asking your other devices to verify this one…", verifyEndToast),
		func() tea.Msg { return svc.Request() },
	)
}
