//go:build matrixe2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/synapseadmin"

	_ "modernc.org/sqlite"
)

// Timeout bounds every request a helper makes and every wait it does.
const Timeout = 60 * time.Second

// Server is the throwaway homeserver.
type Server struct {
	// URL is the client-server API base URL, e.g. http://127.0.0.1:54321.
	URL string
	// Secret is its registration_shared_secret.
	Secret string
}

var (
	serverOnce sync.Once
	server     Server
	serverErr  error
)

// Homeserver returns the test homeserver: the one named by
// MATRIX_E2E_HOMESERVER and MATRIX_E2E_SHARED_SECRET, or else the one
// scripts/matrix-dev/synapse.sh starts (or is already running). It fails
// the test when there is none.
func Homeserver(t testing.TB) Server {
	t.Helper()
	serverOnce.Do(func() { server, serverErr = findServer() })
	if serverErr != nil {
		t.Fatalf("e2e: no test homeserver: %v", serverErr)
	}
	return server
}

func findServer() (Server, error) {
	if u := os.Getenv("MATRIX_E2E_HOMESERVER"); u != "" {
		s := Server{URL: u, Secret: os.Getenv("MATRIX_E2E_SHARED_SECRET")}
		if s.Secret == "" {
			return Server{}, errors.New("MATRIX_E2E_HOMESERVER is set but MATRIX_E2E_SHARED_SECRET is not")
		}
		return s, nil
	}
	script, err := scriptPath()
	if err != nil {
		return Server{}, err
	}
	if out, err := exec.Command(script, "start").CombinedOutput(); err != nil {
		return Server{}, fmt.Errorf("%s start: %v\n%s", script, err, out)
	}
	out, err := exec.Command(script, "env").Output()
	if err != nil {
		return Server{}, fmt.Errorf("%s env: %v", script, err)
	}
	var s Server
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimPrefix(sc.Text(), "export "), "=")
		if !ok {
			continue
		}
		switch k {
		case "MATRIX_E2E_HOMESERVER":
			s.URL = v
		case "MATRIX_E2E_SHARED_SECRET":
			s.Secret = v
		}
	}
	if s.URL == "" || s.Secret == "" {
		return Server{}, fmt.Errorf("%s env printed no server: %q", script, out)
	}
	return s, nil
}

// scriptPath finds scripts/matrix-dev/synapse.sh from the working
// directory (a package directory under go test) upwards.
func scriptPath() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, "scripts", "matrix-dev", "synapse.sh")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("scripts/matrix-dev/synapse.sh not found above the working directory")
		}
		dir = parent
	}
}

// Ctx is a context bounded by Timeout and cancelled when the test ends.
func Ctx(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	t.Cleanup(cancel)
	return ctx
}

// RegisterUser registers a new user on srv through the shared-secret admin
// API and returns its ID and password. The localpart is prefix plus a random
// suffix, so a reused server never collides with an earlier run.
func RegisterUser(t testing.TB, srv Server, prefix string) (id.UserID, string) {
	t.Helper()
	cli, err := mautrix.NewClient(srv.URL, "", "")
	if err != nil {
		t.Fatalf("e2e: client: %v", err)
	}
	localpart := strings.ToLower(prefix) + "-" + randomHex(4)
	password := randomHex(16)
	admin := synapseadmin.Client{Client: cli}
	resp, err := admin.SharedSecretRegister(Ctx(t), srv.Secret, synapseadmin.ReqSharedSecretRegister{
		Username:     localpart,
		Password:     password,
		InhibitLogin: true,
	})
	if err != nil {
		t.Fatalf("e2e: register %s: %v", localpart, err)
	}
	return resp.UserID, password
}

// OpenDB opens a SQLite database at path the way slk's Matrix store does
// (modernc driver, a 5s busy timeout, foreign keys, WAL, immediate
// transactions), wrapped for mautrix. It is closed when the test ends.
// Without the busy timeout, a sync goroutine's writes and a send's fail
// each other with SQLITE_BUSY.
func OpenDB(t testing.TB, path string) *dbutil.Database {
	t.Helper()
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("e2e: open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openDB(path string) (*dbutil.Database, error) {
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return db, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Eventually polls cond every 100ms until it is true, failing the test
// after Timeout.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(Timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("e2e: timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
