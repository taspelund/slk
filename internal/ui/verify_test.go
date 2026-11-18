package ui

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/core"
)

// fakeVerify records the answers the App sends through
// core.VerificationService, as "accept t1", "confirm t1", "cancel t1
// mismatch=true", "request".
type fakeVerify struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeVerify) record(s string) core.Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return nil
}

func (f *fakeVerify) Request() core.Msg          { return f.record("request") }
func (f *fakeVerify) Accept(id string) core.Msg  { return f.record("accept " + id) }
func (f *fakeVerify) Confirm(id string) core.Msg { return f.record("confirm " + id) }
func (f *fakeVerify) Cancel(id string, mismatch bool) core.Msg {
	return f.record(fmt.Sprintf("cancel %s mismatch=%v", id, mismatch))
}

func (f *fakeVerify) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// verifyApp is an 80x24 App in normal mode whose verification answers are
// recorded, and whose toasts stay until replaced (toastTick never fires).
func verifyApp(t *testing.T) (*App, *fakeVerify) {
	t.Helper()
	a := newTestApp(t, withSize(80, 24))
	old := toastTick
	toastTick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	t.Cleanup(func() { toastTick = old })
	f := &fakeVerify{}
	a.SetVerificationService(f)
	return a, f
}

// feedAll delivers msg through Update and feeds every message its commands
// produce back through Update, until none are left.
func feedAll(t *testing.T, a *App, msg tea.Msg) {
	t.Helper()
	queue := []tea.Msg{msg}
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		if m == nil {
			continue
		}
		_, cmd := a.Update(m)
		queue = append(queue, drainBatch(cmd)...)
	}
}

// runColonCommand types :line and Enter through Update, feeding back every
// message the keys produce.
func runColonCommand(t *testing.T, a *App, line string) {
	t.Helper()
	pressKeyAll(t, a, keyPress(':'))
	for _, r := range line {
		pressKeyAll(t, a, keyPress(r))
	}
	pressKeyAll(t, a, keyCode(tea.KeyEnter))
}

var sevenEmoji = []VerifyEmoji{
	{"🐶", "Dog"}, {"🐱", "Cat"}, {"🦁", "Lion"}, {"🐎", "Horse"},
	{"🦄", "Unicorn"}, {"🐷", "Pig"}, {"🐘", "Elephant"},
}

func TestVerifyIncomingAcceptMatchDone(t *testing.T) {
	a, f := verifyApp(t)
	feedAll(t, a, VerifyRequestMsg{ID: "t1", Device: "Element (ABC)"})
	if a.mode != ModeConfirm || !dialogShows(a, "Element (ABC) asks to verify") || !dialogShows(a, "[y] Accept") {
		t.Fatalf("mode = %v; want the request dialog:\n%s", a.mode, a.confirmPrompt.View())
	}
	pressKeyAll(t, a, keyPress('y'))
	if a.confirmPrompt.IsVisible() || a.mode != ModeNormal {
		t.Errorf("the request dialog stayed open after Accept (mode %v)", a.mode)
	}

	feedAll(t, a, VerifyEmojiMsg{ID: "t1", Device: "Element (ABC)", Emoji: sevenEmoji})
	for _, want := range []string{"🐶  Dog", "🐘  Elephant", "[y] They match", "[n] They don't match", "[c] Cancel"} {
		if !dialogShows(a, want) {
			t.Errorf("the emoji dialog lacks %q:\n%s", want, a.confirmPrompt.View())
		}
	}
	pressKeyAll(t, a, keyPress('y'))
	feedAll(t, a, VerifyEndMsg{ID: "t1", Text: "Verified with Element (ABC)"})
	if !toastShows(a, "Verified with Element (ABC)") {
		t.Errorf("toast = %q", statusbarText(a))
	}
	if want := []string{"accept t1", "confirm t1"}; !slices.Equal(f.got(), want) {
		t.Errorf("answers = %v, want %v", f.got(), want)
	}
}

func TestVerifyAnswersThatEndIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		emoji bool
		key   tea.KeyMsg
		want  string
	}{
		{"decline with n", false, keyPress('n'), "cancel t1 mismatch=false"},
		{"decline with Esc", false, keyCode(tea.KeyEscape), "cancel t1 mismatch=false"},
		{"emoji don't match", true, keyPress('n'), "cancel t1 mismatch=true"},
		{"emoji cancel", true, keyPress('c'), "cancel t1 mismatch=false"},
		{"emoji Esc", true, keyCode(tea.KeyEscape), "cancel t1 mismatch=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f := verifyApp(t)
			if tc.emoji {
				feedAll(t, a, VerifyEmojiMsg{ID: "t1", Device: "phone", Emoji: sevenEmoji})
			} else {
				feedAll(t, a, VerifyRequestMsg{ID: "t1", Device: "phone"})
			}
			pressKeyAll(t, a, tc.key)
			if a.confirmPrompt.IsVisible() {
				t.Error("the dialog stayed open")
			}
			if got := f.got(); !slices.Equal(got, []string{tc.want}) {
				t.Errorf("answers = %v, want [%s]", got, tc.want)
			}
		})
	}
}

// A request that arrives while another dialog is open waits, says so once,
// and opens when that dialog closes.
func TestVerifyRequestWaitsBehindAnotherDialog(t *testing.T) {
	a, _ := verifyApp(t)
	a.openHelpOverlay()
	feedAll(t, a, VerifyRequestMsg{ID: "t1", Device: "phone"})
	if a.mode != ModeHelp || a.confirmPrompt.IsVisible() {
		t.Fatalf("the request interrupted the help overlay (mode %v)", a.mode)
	}
	if !toastShows(a, "Verification request from phone: :verify to answer") {
		t.Errorf("toast = %q", statusbarText(a))
	}
	pressKeyAll(t, a, keyCode(tea.KeyEscape)) // close help
	if a.mode != ModeConfirm || !dialogShows(a, "phone asks to verify") {
		t.Errorf("mode = %v; the request should open once help closes", a.mode)
	}
}

// While the user types, the dialog waits; :verify opens it.
func TestVerifyWaitsWhileTypingAndVerifyCommandOpensIt(t *testing.T) {
	a, f := verifyApp(t)
	a.SetMode(ModeInsert)
	a.compose.SetValue("half a thought")
	feedAll(t, a, VerifyEmojiMsg{ID: "t1", Device: "phone", Emoji: sevenEmoji})
	if a.mode != ModeInsert || a.confirmPrompt.IsVisible() {
		t.Fatal("the emoji dialog interrupted typing")
	}
	a.SetMode(ModeNormal)
	runColonCommand(t, a, "verify")
	if a.mode != ModeConfirm || !dialogShows(a, "🐶  Dog") {
		t.Fatalf("mode = %v; :verify should open the waiting dialog", a.mode)
	}
	if len(f.got()) != 0 {
		t.Errorf(":verify with a dialog waiting sent %v", f.got())
	}
}

// A later step of the same verification replaces its waiting dialog; the
// end of a verification drops its waiting dialog and closes a shown one.
func TestVerifyLaterStepReplacesAndEndCloses(t *testing.T) {
	a, _ := verifyApp(t)
	a.openHelpOverlay()
	feedAll(t, a, VerifyRequestMsg{ID: "t1", Device: "phone"})
	feedAll(t, a, VerifyEmojiMsg{ID: "t1", Device: "phone", Emoji: sevenEmoji})
	feedAll(t, a, VerifyRequestMsg{ID: "t2", Device: "laptop"})
	if n := len(a.verify.queue); n != 2 {
		t.Fatalf("queue holds %d dialogs, want 2 (t1's emoji, t2's request)", n)
	}
	feedAll(t, a, VerifyEndMsg{ID: "t1", Text: "Verification with phone timed out"})
	if !toastShows(a, "timed out") {
		t.Errorf("toast = %q", statusbarText(a))
	}
	pressKeyAll(t, a, keyCode(tea.KeyEscape)) // close help
	if !dialogShows(a, "laptop asks to verify") {
		t.Fatalf("want t2's request, got:\n%s", a.confirmPrompt.View())
	}
	feedAll(t, a, VerifyEndMsg{ID: "t2", Text: "Verification cancelled"})
	if a.confirmPrompt.IsVisible() || a.mode != ModeNormal {
		t.Errorf("the shown dialog stayed open after its verification ended (mode %v)", a.mode)
	}
}

// A dialog that opened over insert mode with an empty composer puts the
// user back to typing when it closes.
func TestVerifyDialogReturnsToInsert(t *testing.T) {
	a, _ := verifyApp(t)
	a.SetMode(ModeInsert)
	feedAll(t, a, VerifyRequestMsg{ID: "t1", Device: "phone"})
	if a.mode != ModeConfirm {
		t.Fatalf("mode = %v; an empty composer is not busy", a.mode)
	}
	pressKeyAll(t, a, keyPress('n'))
	if a.mode != ModeInsert {
		t.Errorf("mode = %v, want back in insert", a.mode)
	}
}

func TestVerifyCommandRequests(t *testing.T) {
	a, f := verifyApp(t)
	runColonCommand(t, a, "verify")
	if got := f.got(); !slices.Equal(got, []string{"request"}) {
		t.Errorf("answers = %v, want [request]", got)
	}
	if !toastShows(a, "Asking your other devices") {
		t.Errorf("toast = %q", statusbarText(a))
	}
}

func TestVerifyCommandWithoutService(t *testing.T) {
	a, _ := verifyApp(t)
	a.SetVerificationService(nil)
	runColonCommand(t, a, "verify")
	if got := statusbarText(a); !strings.Contains(got, "Device verification is for Matrix accounts") {
		t.Errorf("toast = %q", got)
	}
}
