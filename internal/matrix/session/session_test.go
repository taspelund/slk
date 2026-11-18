package session

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func testCreds() Credentials {
	return Credentials{
		Homeserver:  "https://matrix.example.org",
		UserID:      "@alice:example.org",
		DeviceID:    "ABCDEFGHIJ",
		AccessToken: "syt_secret",
		PickleKey:   []byte("0123456789abcdef0123456789abcdef"),
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "matrix", "work")
	want := testCreds()
	if err := SaveCredentials(dir, want); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	got, err := LoadCredentials(dir)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	// Saving again replaces the file and leaves no temporary behind.
	want.AccessToken = "syt_newer"
	if err := SaveCredentials(dir, want); err != nil {
		t.Fatalf("SaveCredentials again: %v", err)
	}
	if got, _ := LoadCredentials(dir); got.AccessToken != "syt_newer" {
		t.Errorf("after second save, token = %q", got.AccessToken)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "session.json" {
		t.Errorf("dir holds %v, want only session.json", entries)
	}
}

func TestSaveCredentialsModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes on Windows")
	}
	dir := filepath.Join(t.TempDir(), "acct")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveCredentials(dir, testCreds()); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %04o, want 0700", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Join(dir, "session.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %04o, want 0600", info.Mode().Perm())
	}

	fresh := filepath.Join(t.TempDir(), "a", "b")
	if err := SaveCredentials(fresh, testCreds()); err != nil {
		t.Fatalf("SaveCredentials into new dirs: %v", err)
	}
	if info, _ := os.Stat(fresh); info.Mode().Perm() != 0o700 {
		t.Errorf("new dir mode = %04o, want 0700", info.Mode().Perm())
	}
}

func TestLoadCredentialsRejectsOpenFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes on Windows")
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o620} {
		dir := t.TempDir()
		if err := SaveCredentials(dir, testCreds()); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "session.json")
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCredentials(dir); err == nil {
			t.Errorf("mode %04o: LoadCredentials succeeded, want a refusal", mode)
		}
	}
}

func TestLoadCredentialsErrors(t *testing.T) {
	if _, err := LoadCredentials(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v, want fs.ErrNotExist", err)
	}

	dir := t.TempDir()
	c := testCreds()
	c.AccessToken = ""
	data, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(dir, "session.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(dir); err == nil {
		t.Error("file without an access token loaded")
	}
	if err := SaveCredentials(t.TempDir(), c); err == nil {
		t.Error("credentials without an access token saved")
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := backoff(1000); got != maxBackoff {
		t.Errorf("backoff(1000) = %v, want %v", got, maxBackoff)
	}
}

func TestSyncFilterJSON(t *testing.T) {
	data, err := json.Marshal(syncFilter())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"room":{"ephemeral":{"types":["m.receipt","m.typing"]},` +
		`"state":{"lazy_load_members":true},` +
		`"timeline":{"limit":50,"lazy_load_members":true,"unread_thread_notifications":true}}}`
	if string(data) != want {
		t.Errorf("filter =\n%s\nwant\n%s", data, want)
	}
}

func TestFatalErrors(t *testing.T) {
	httpErr := func(code string, extra map[string]any) error {
		extra["errcode"] = code
		return mautrix.HTTPError{RespError: &mautrix.RespError{ErrCode: code, ExtraData: extra}}
	}
	var lo *LoggedOutError
	if err := fatal(httpErr("M_UNKNOWN_TOKEN", map[string]any{"soft_logout": true})); !errors.As(err, &lo) || !lo.SoftLogout {
		t.Errorf("soft logout: fatal = %v", err)
	}
	if err := fatal(httpErr("M_UNKNOWN_TOKEN", map[string]any{})); !errors.As(err, &lo) || lo.SoftLogout {
		t.Errorf("hard logout: fatal = %v", err)
	} else if !errors.Is(err, mautrix.MUnknownToken) {
		t.Errorf("LoggedOutError does not wrap M_UNKNOWN_TOKEN: %v", err)
	}
	if err := fatal(httpErr("M_MISSING_TOKEN", map[string]any{})); err == nil {
		t.Error("M_MISSING_TOKEN is not fatal")
	}
	limited := httpErr("M_LIMIT_EXCEEDED", map[string]any{"retry_after_ms": float64(45000)})
	if err := fatal(limited); err != nil {
		t.Errorf("rate limit is fatal: %v", err)
	}
	if got := retryAfter(limited); got != 45*time.Second {
		t.Errorf("retryAfter = %v, want 45s", got)
	}
	if err := fatal(errors.New("connection refused")); err != nil {
		t.Errorf("network error is fatal: %v", err)
	}
}

func encryptedEvent(evtID id.EventID, session id.SessionID) *event.Event {
	return &event.Event{
		ID:      evtID,
		RoomID:  "!room:example.org",
		Type:    event.EventEncrypted,
		Content: event.Content{Parsed: &event.EncryptedEventContent{SessionID: session}},
	}
}

func TestPendingKeys(t *testing.T) {
	p := newPendingKeys()
	woke := func() bool {
		select {
		case <-p.wake:
			return true
		default:
			return false
		}
	}

	p.received("S1") // not running: ignored
	p.setRunning(true)
	p.add(encryptedEvent("$a", "S1"))
	p.add(encryptedEvent("$b", "S1"))
	p.add(encryptedEvent("$c", "S2"))
	if got := p.takeArrived(); len(got) != 0 {
		t.Fatalf("arrival before Run counted: %v", got)
	}

	// An arrival with nothing waiting is remembered but does not wake.
	p.received("S3")
	if woke() {
		t.Error("woke for a session nothing waits for")
	}
	p.add(encryptedEvent("$d", "S3"))
	p.received("S1")
	got := p.takeArrived()
	ids := map[id.EventID]bool{}
	for _, evt := range got {
		ids[evt.ID] = true
	}
	if len(got) != 3 || !ids["$a"] || !ids["$b"] || !ids["$d"] {
		t.Errorf("takeArrived = %v, want $a $b $d", ids)
	}
	if woke() {
		t.Error("takeArrived left a wake-up behind")
	}
	p.received("S2")
	if !woke() {
		t.Error("no wake-up for a session with a waiting event")
	}
	if got := p.takeArrived(); len(got) != 1 || got[0].ID != "$c" || p.n != 0 {
		t.Errorf("takeArrived = %v (n=%d), want $c (n=0)", got, p.n)
	}

	for i := range maxPending {
		if !p.add(encryptedEvent(id.EventID("$x"+string(rune(i))), "S4")) {
			t.Fatalf("add %d refused below the cap", i)
		}
	}
	if p.add(encryptedEvent("$over", "S4")) {
		t.Error("add accepted past maxPending")
	}
}
