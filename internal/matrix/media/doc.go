// Package media fetches, decrypts, caches and uploads Matrix media for an
// account: authenticated mxc downloads (/_matrix/client/v1/media), the
// encrypted attachments of encrypted rooms, and uploads. svc/files.go
// adapts it to internal/core's FileService and ImageFetcher (WP-Media).
//
// # URLs the UI holds
//
// A core.Attachment never carries an mxc URI or a key. Its DownloadURL
// and thumbnail URLs are Ref URLs (matrix-media://slk/<part>?room=…&event=…),
// which name a stored message and which of its files to read: the file
// itself, the sender's thumbnail, or a server-scaled thumbnail. Reading
// one looks the message up in the account database (Lookup), so the key
// of an encrypted file stays in the database, never in a URL, a log line
// or a map that only lives as long as the process. Its URL is the
// message's matrix.to permalink, which the UI shows and opens.
//
// # Cache layout, under the account directory
//
//	media/images/<FileID>-<px>.<ext>   inline images and previews, decrypted:
//	                                   internal/image's LRU disk cache, capped
//	                                   at ImageCacheMB
//	media/files/<hash>/<name>          files saved by Download, decrypted, named
//	                                   for the user; indexed in
//	                                   slkmx_media_cache_index (mxc, path, size,
//	                                   last_used), least recently used evicted
//	                                   past FileCacheBytes
//
// Both hold plaintext, as matrix.db holds decrypted message text; the
// directories are 0700 and the files 0600.
package media
