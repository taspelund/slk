package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// credentialsFile is the credentials' name in the account directory.
const credentialsFile = "session.json"

func loadCredentials(dir string) (Credentials, error) {
	path := filepath.Join(dir, credentialsFile)
	f, err := os.Open(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("session: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Credentials{}, fmt.Errorf("session: %w", err)
	}
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm&0o077 != 0 {
		return Credentials{}, fmt.Errorf("session: %s has mode %04o, readable or writable by others; it holds an access token, so run: chmod 600 %s", path, perm, path)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return Credentials{}, fmt.Errorf("session: read %s: %w", path, err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return Credentials{}, fmt.Errorf("session: parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return Credentials{}, fmt.Errorf("session: %s: %w", path, err)
	}
	return c, nil
}

func (c Credentials) validate() error {
	switch {
	case c.Homeserver == "":
		return errors.New("no homeserver")
	case c.UserID == "":
		return errors.New("no user ID")
	case c.DeviceID == "":
		return errors.New("no device ID")
	case c.AccessToken == "":
		return errors.New("no access token")
	case len(c.PickleKey) == 0:
		return errors.New("no pickle key")
	}
	return nil
}

func saveCredentials(dir string, c Credentials) error {
	if err := c.validate(); err != nil {
		return fmt.Errorf("session: credentials: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	// CreateTemp makes the file 0600; the rename replaces the old file in
	// one step, so no reader sees a partial one.
	tmp, err := os.CreateTemp(dir, ".session-*.json")
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer os.Remove(tmp.Name()) // fails harmlessly once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("session: write credentials: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("session: write credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: write credentials: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, credentialsFile)); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}
