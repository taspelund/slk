package media

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Download saves the file the Ref URL rawURL names under media/files, as
// name, and returns its path. A file saved before is not downloaded
// again while it is cached.
//
// slkmx_media_cache_index has a row per saved file, keyed by its mxc URI
// (the ciphertext's, for an encrypted file): its path, size and when it
// was last used. Past FileCacheBytes, the least recently used files go.
func (c *Client) Download(ctx context.Context, rawURL, name string) (string, error) {
	ref, ok := ParseURL(rawURL)
	if !ok || ref.Part != PartFile {
		return "", fmt.Errorf("media: not a file URL: %q", rawURL)
	}
	src, err := c.source(ctx, ref)
	if err != nil {
		return "", err
	}
	key := src.MXC.String()
	if path, ok, err := c.cached(ctx, key); err != nil || ok {
		return path, err
	}
	data, _, err := c.read(ctx, src)
	if err != nil {
		return "", err
	}
	path, err := c.save(key, name, data)
	if err != nil {
		return "", err
	}
	if _, err := c.db.Exec(ctx, `INSERT INTO slkmx_media_cache_index (mxc, path, size, last_used)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (mxc) DO UPDATE SET path = excluded.path, size = excluded.size, last_used = excluded.last_used`,
		key, path, len(data), c.now().UnixMilli()); err != nil {
		return "", fmt.Errorf("media: index %s: %w", key, err)
	}
	if err := c.evict(ctx, key); err != nil {
		return "", err
	}
	return path, nil
}

// cached is the saved file of key, if its index row and the file are
// both there; it marks the file used.
func (c *Client) cached(ctx context.Context, key string) (string, bool, error) {
	var path string
	err := c.db.QueryRow(ctx, `SELECT path FROM slkmx_media_cache_index WHERE mxc = $1`, key).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("media: index %s: %w", key, err)
	}
	if _, err := os.Stat(path); err != nil {
		return "", false, nil // gone from disk: download it again
	}
	if _, err := c.db.Exec(ctx, `UPDATE slkmx_media_cache_index SET last_used = $2 WHERE mxc = $1`,
		key, c.now().UnixMilli()); err != nil {
		return "", false, fmt.Errorf("media: index %s: %w", key, err)
	}
	return path, true, nil
}

// save writes data as media/files/<hash of key>/<name>, replacing what
// was there.
func (c *Client) save(key, name string, data []byte) (string, error) {
	sum := sha256.Sum256([]byte(key))
	dir := filepath.Join(c.dir, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("media: %w", err)
	}
	path := filepath.Join(dir, safeName(name))
	tmp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return "", fmt.Errorf("media: %w", err)
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil && cerr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("media: saving %s: %w", name, errors.Join(werr, cerr))
	}
	return path, nil
}

// evict removes the least recently used saved files, never keep, until
// the rest fit in the cap.
func (c *Client) evict(ctx context.Context, keep string) error {
	var total int64
	if err := c.db.QueryRow(ctx, `SELECT COALESCE(SUM(size), 0) FROM slkmx_media_cache_index`).Scan(&total); err != nil {
		return fmt.Errorf("media: index: %w", err)
	}
	if total <= c.fileCap {
		return nil
	}
	rows, err := c.db.Query(ctx, `SELECT mxc, path, size FROM slkmx_media_cache_index
		WHERE mxc <> $1 ORDER BY last_used, mxc`, keep)
	if err != nil {
		return fmt.Errorf("media: index: %w", err)
	}
	type entry struct {
		key, path string
		size      int64
	}
	var drop []entry
	for rows.Next() && total > c.fileCap {
		var e entry
		if err := rows.Scan(&e.key, &e.path, &e.size); err != nil {
			rows.Close()
			return fmt.Errorf("media: index: %w", err)
		}
		drop = append(drop, e)
		total -= e.size
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return fmt.Errorf("media: index: %w", err)
	}
	for _, e := range drop {
		if err := os.Remove(e.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("media: evicting %s: %w", e.path, err)
		}
		_ = os.Remove(filepath.Dir(e.path)) // only when empty
		if _, err := c.db.Exec(ctx, `DELETE FROM slkmx_media_cache_index WHERE mxc = $1`, e.key); err != nil {
			return fmt.Errorf("media: index: %w", err)
		}
	}
	return nil
}

// safeName makes a file's display name safe as a file name: separators
// and control characters become '_', leading dots and surrounding space
// go, and nothing left is "download". (internal/filedl's sanitizeName
// for Slack files, which is unexported.)
func safeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	name = strings.TrimLeft(name, ".")
	if name == "" {
		return "download"
	}
	return name
}
