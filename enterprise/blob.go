package enterprise

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"
)

func (s *Service) readOriginalBlob(ctx context.Context, key string) ([]byte, error) {
	if !filepath.IsAbs(key) {
		return s.blobs.Get(ctx, key)
	}
	// Existing rows identify objects by their absolute path in the local store.
	relative, err := filepath.Rel(s.Restricted, key)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(key) != key || !filepath.IsLocal(relative) {
		return nil, fmt.Errorf("original is outside the restricted store: %w", fs.ErrInvalid)
	}
	return s.localBlobs.Get(ctx, filepath.ToSlash(relative))
}

func (s *Service) writeIssue(ctx context.Context, tx *sql.Tx, versionID, issuer, hash, filename string, file []byte, issuedAt time.Time) error {
	key := "issues/" + versionID + "/" + hash
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ent_issues (version_id, issuer_id, file_hash, blob_key, filename, issued_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, versionID, issuer, hash, key, filename, issuedAt); err != nil {
		return err
	}
	return s.blobs.Put(ctx, key, file)
}
