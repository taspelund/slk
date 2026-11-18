package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"golang.org/x/term"
	"maunium.net/go/mautrix"
	_ "modernc.org/sqlite"

	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
)

// The Matrix backend's entry points, behind hidden flags while it is being
// built (see internal/matrix):
//
//	slk --matrix-login <account>   log in and save the device
//	slk --matrix <account>         run the TUI against that account
//
// Each account's credentials and database live in matrixAccountDir.

// matrixAccountDir is where account keeps session.json and matrix.db.
func matrixAccountDir(account string) string {
	return filepath.Join(xdgData(), "matrix", account)
}

// runMatrixLogin asks for a homeserver, user and password, logs in and
// saves the credentials. Then it offers to verify the new device with the
// account's recovery key, which also restores room keys from the key
// backup so that history from before this device can be read.
//
// It refuses an account that is already logged in: its matrix.db belongs
// to the old device, and a new login is a new device.
func runMatrixLogin(account string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	dir := matrixAccountDir(account)
	for _, name := range []string{"session.json", "matrix.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("account %q is already logged in (%s); remove that directory to log in again as a new device", account, dir)
		}
	}
	in := bufio.NewReader(os.Stdin)

	server, err := promptLine(in, "Homeserver (e.g. matrix.org or https://matrix.example.org): ")
	if err != nil {
		return err
	}
	homeserver, err := resolveHomeserver(ctx, server)
	if err != nil {
		return err
	}
	user, err := promptLine(in, "User ID (@user:server, or just user): ")
	if err != nil {
		return err
	}
	password, err := promptSecret(in, "Password: ")
	if err != nil {
		return err
	}
	deviceName := "slk"
	if host, err := os.Hostname(); err == nil && host != "" {
		deviceName = fmt.Sprintf("slk (%s)", host)
	}
	creds, err := session.Login(ctx, homeserver, user, password, deviceName)
	if err != nil {
		return err
	}
	if err := session.SaveCredentials(dir, creds); err != nil {
		return err
	}
	fmt.Printf("Logged in as %s on %s.\nDevice ID: %s (%q)\n", creds.UserID, creds.Homeserver, creds.DeviceID, deviceName)

	key, err := promptSecret(in, "Recovery key, to verify this device and restore message history (Enter to skip): ")
	if err != nil {
		return err
	}
	if key == "" {
		fmt.Println("Skipped. Without verification, encrypted messages from before this login stay unreadable.")
		return nil
	}
	st, err := store.Open(ctx, filepath.Join(dir, "matrix.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := session.New(ctx, creds, st, session.DebugLogger())
	if err != nil {
		return err
	}
	fmt.Println("Verifying and restoring room keys…")
	switch err := s.VerifyWithRecoveryKey(ctx, key); {
	case errors.Is(err, session.ErrNoKeyBackup):
		fmt.Println("Device verified. The account has no key backup to restore room keys from.")
	case err != nil:
		return fmt.Errorf("%w (you are logged in; the device is not verified)", err)
	default:
		fmt.Println("Device verified; room keys restored from the key backup.")
	}
	return nil
}

// resolveHomeserver turns what the user typed into a client-server API
// base URL: a URL is taken as is; a server name is looked up in its
// .well-known/matrix/client, falling back to https://<name>. Either way it
// checks that a Matrix server answers there.
func resolveHomeserver(ctx context.Context, server string) (string, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		return "", errors.New("no homeserver given")
	}
	base := server
	if !strings.HasPrefix(server, "https://") && !strings.HasPrefix(server, "http://") {
		base = "https://" + server
		wk, err := mautrix.DiscoverClientAPI(ctx, server)
		if err == nil && wk != nil && wk.Homeserver.BaseURL != "" {
			base = wk.Homeserver.BaseURL
		}
	}
	base = strings.TrimRight(base, "/")
	cli, err := mautrix.NewClient(base, "", "")
	if err != nil {
		return "", fmt.Errorf("homeserver %q: %w", base, err)
	}
	if _, err := cli.Versions(ctx); err != nil {
		return "", fmt.Errorf("no Matrix homeserver at %s: %w", base, err)
	}
	return base, nil
}

// promptLine prints prompt and reads one line from in.
func promptLine(in *bufio.Reader, prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", fmt.Errorf("reading input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// promptSecret is promptLine without echo when stdin is a terminal.
func promptSecret(in *bufio.Reader, prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return promptLine(in, prompt)
	}
	fmt.Print(prompt)
	b, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading input: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// runMatrix runs slk against account, wired the way runDemo wires
// internal/demo.
func runMatrix(account string) error {
	return errors.New("--matrix is not implemented yet")
}
