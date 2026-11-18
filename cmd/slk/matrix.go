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
	"sync"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"
	"maunium.net/go/mautrix"
	_ "modernc.org/sqlite"

	"github.com/gammons/slk/internal/config"
	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/debuglog"
	"github.com/gammons/slk/internal/export"
	imgpkg "github.com/gammons/slk/internal/image"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/matrix/svc"
	"github.com/gammons/slk/internal/notify"
	"github.com/gammons/slk/internal/ui"
	"github.com/gammons/slk/internal/ui/imgrender"
	"github.com/gammons/slk/internal/ui/messages"
	"github.com/gammons/slk/internal/ui/styles"
	"github.com/gammons/slk/internal/ui/workspace"
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
	dir := matrixAccountDir(account)
	creds, err := session.LoadCredentials(dir)
	if err != nil {
		return fmt.Errorf("account %q: %w (log in with slk --matrix-login %s)", account, err, account)
	}

	cfg, err := config.Load(filepath.Join(xdgConfig(), "config.toml"))
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	messages.SetDateTokenTimeLayout(cfg.Appearance.TimestampFormat)
	styles.LoadCustomThemes(os.DirFS(filepath.Join(xdgConfig(), "themes")))
	styles.SetTransparentBackground(cfg.Appearance.TransparentBackground)
	styles.Apply(cfg.Appearance.Theme, cfg.Theme)
	styles.ApplyCompact(cfg.Appearance.Compact)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.Open(ctx, filepath.Join(dir, "matrix.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	sess, err := session.New(ctx, creds, st, session.DebugLogger())
	if err != nil {
		return err
	}
	ids := ident.New(st)
	model, err := rooms.New(ctx, st, ids, creds.UserID)
	if err != nil {
		return err
	}
	b := svc.New(svc.Deps{
		Dir:             dir,
		Account:         account,
		Session:         sess,
		Store:           st,
		IDs:             ids,
		Rooms:           model,
		TimestampFormat: cfg.Appearance.TimestampFormat,
	})

	app := ui.NewApp()
	applyUISettings(app, uiSettingsFromConfig(cfg))
	app.SetLoadingWorkspaces([]string{account})
	app.SetWorkspaces([]workspace.WorkspaceItem{{ID: b.TeamID(), Name: account, Initials: workspace.WorkspaceInitials(account)}})
	b.Install(app)
	b.SetNotifications(svc.Notifications{
		Notifier:        notify.New(cfg.Notifications.Enabled, cfg.Notifications.NotifyCommand),
		Config:          cfg.Notifications,
		ActiveChannelID: app.ActiveChannelID,
	})
	// Files open with the OS (downloads), as on Slack.
	app.SetDesktopService(core.NewDesktopService(core.DesktopServiceFuncs{
		Open:       launchOS,
		Stat:       os.Stat,
		SaveThread: export.SaveThread,
	}))
	// Inline images draw as half blocks, which work in every terminal
	// and multiplexer. Kitty and sixel need main.go's terminal probes and
	// frame output, which are not shared with this path yet.
	imgCtx := func(send func(tea.Msg)) imgrender.ImageContext {
		return imgrender.ImageContext{
			Protocol: imgpkg.ProtoHalfBlock,
			Fetcher:  b.ImageFetcher(),
			MaxRows:  cfg.Appearance.MaxImageRows,
			MaxCols:  cfg.Appearance.MaxImageCols,
			SendMsg:  send,
		}
	}
	app.SetImageProtocol(imgpkg.ProtoHalfBlock)
	app.SetImageContext(imgCtx(nil))
	p := tea.NewProgram(app)
	app.SetImageContext(imgCtx(p.Send))
	// Presence toasts and loaded member lists reach the UI outside Run.
	b.SetUISend(p.Send)

	// The session syncs and the backend applies what it syncs from the
	// start, but the UI hears nothing until the workspace exists: Run
	// holds its sends until StartupMsgs returns, so they wait behind gate.
	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := sess.Run(ctx); err != nil && ctx.Err() == nil {
			debuglog.General("matrix: sync stopped: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		_ = b.Run(ctx, sess.Events(), func(m tea.Msg) {
			select {
			case <-gate:
				p.Send(m)
			case <-ctx.Done():
			}
		})
	}()
	go func() {
		msgs, err := b.StartupMsgs(ctx)
		if err != nil {
			debuglog.General("matrix: startup: %v", err)
		}
		for _, m := range msgs {
			if ready, ok := m.(ui.WorkspaceReadyMsg); ok {
				ready.Theme = cfg.ResolveTheme(ready.TeamID)
				ready.SidebarWidth = cfg.ResolveWidth(ready.TeamID)
				m = ready
			}
			p.Send(m)
		}
		close(gate)
	}()

	_, err = p.Run()
	cancel()
	wg.Wait()
	return err
}
