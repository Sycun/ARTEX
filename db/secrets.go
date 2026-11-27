package db

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Secrets at rest for capture-proxy rewrite rules and tamper hooks.
// TEXT stores "enc:v1:<base64(nonce||ciphertext)>". JSONB stores
// {"__artex_enc":"enc:v1:..."} over the canonical JSON. Empty values stay empty.
// A value that fails to decrypt is an error, never returned as-is.
//
// This sealer covers the columns a hook or rewrite action can use to store a
// secret. Other application columns stay in clear so existing readers are unchanged.

const (
	secretPrefix  = "enc:v1:"
	sealedJSONKey = "__artex_enc"
	secretKeyEnv  = "ARTEX_SECRET_KEY"
	secretKeyFile = "secret.key"
)

type secretColumn struct {
	table, col string
	json       bool
}

func (c secretColumn) aad() string { return c.table + "." + c.col }

func (c secretColumn) idCol() string { return "id" }

func (c secretColumn) valueExpr() string {
	if c.json {
		return c.col + "->>'" + sealedJSONKey + "'"
	}
	return c.col
}

var (
	colRewriteMatchers  = secretColumn{"traffic_rewrite_rules", "matchers", true}
	colRewriteActions   = secretColumn{"traffic_rewrite_rules", "actions", true}
	colHookMatchers     = secretColumn{"traffic_tamper_hooks", "matchers", true}
	colHookSource       = secretColumn{"traffic_tamper_hooks", "source", false}
	colHookVerMatchers  = secretColumn{"traffic_tamper_hook_versions", "matchers", true}
	colHookVerSource    = secretColumn{"traffic_tamper_hook_versions", "source", false}
	colChatBridgeConfig = secretColumn{"chat_bridges", "config", true}
	colCredPass         = secretColumn{"task_credentials", "password", false}
	secretColumns       = []secretColumn{
		colRewriteMatchers, colRewriteActions,
		colHookMatchers, colHookSource,
		colHookVerMatchers, colHookVerSource,
		colChatBridgeConfig,
		colCredPass,
	}
)

type secretBox struct{ aead cipher.AEAD }

var activeSecretBox atomic.Pointer[secretBox]

const trafficTraceDomain = "artex-traffic-trace"

var trafficTraceKey atomic.Pointer[[]byte]

// TrafficTraceKey is the in-memory sub-key that fingerprints rewrite-trace
// values. Nil until InitSecrets ran. It is never written to the database.
func TrafficTraceKey() []byte {
	if k := trafficTraceKey.Load(); k != nil {
		return *k
	}
	return nil
}

func deriveSubKey(key []byte, domain string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(domain))
	return m.Sum(nil)
}

func newSecretBox(key []byte) (*secretBox, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	return &secretBox{aead}, err
}

func isEncrypted(v string) bool { return strings.HasPrefix(v, secretPrefix) }

func (b *secretBox) encrypt(aad, plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), []byte(aad))), nil
}

func (b *secretBox) decrypt(aad, v string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, secretPrefix))
	n := b.aead.NonceSize()
	if err != nil || len(raw) < n {
		return "", fmt.Errorf("decrypt %s: malformed ciphertext", aad)
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(aad))
	if err != nil {
		return "", fmt.Errorf("decrypt %s: authentication failed (wrong key or altered value)", aad)
	}
	return string(plain), nil
}

func sealedJSON(raw string) (string, bool) {
	if !strings.Contains(raw, `"`+sealedJSONKey+`"`) {
		return "", false
	}
	var m map[string]json.RawMessage
	var s string
	if json.Unmarshal([]byte(raw), &m) != nil || len(m) != 1 || json.Unmarshal(m[sealedJSONKey], &s) != nil || !isEncrypted(s) {
		return "", false
	}
	return s, true
}

func isSealed(c secretColumn, v string) bool {
	if c.json {
		_, ok := sealedJSON(v)
		return ok
	}
	return isEncrypted(v)
}

func (b *secretBox) seal(c secretColumn, v string) (string, error) {
	if !c.json {
		return b.encrypt(c.aad(), v)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(v)))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return "", fmt.Errorf("%s: invalid JSON", c.aad())
	}
	m, isObject := parsed.(map[string]any)
	if _, reserved := m[sealedJSONKey]; reserved {
		return "", fmt.Errorf("%s: key %q is reserved", c.aad(), sealedJSONKey)
	}
	if parsed == nil || (isObject && len(m) == 0) {
		return v, nil
	}
	canon, err := json.Marshal(parsed)
	if err != nil {
		return "", err
	}
	enc, err := b.encrypt(c.aad(), string(canon))
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(map[string]string{sealedJSONKey: enc})
	return string(out), err
}

func (b *secretBox) open(c secretColumn, v string) (string, error) {
	if c.json {
		v, _ = sealedJSON(v)
	}
	return b.decrypt(c.aad(), v)
}

var errNoSecretKey = errors.New("secrets at rest: key not initialized")

func sealSecret(c secretColumn, v string) (string, error) {
	if b := activeSecretBox.Load(); b != nil {
		return b.seal(c, v)
	}
	return "", errNoSecretKey
}

func openSecret(c secretColumn, v string) (string, error) {
	if !isSealed(c, v) {
		return v, nil
	}
	if b := activeSecretBox.Load(); b != nil {
		return b.open(c, v)
	}
	return "", errNoSecretKey
}

func parseSecretKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if k, err := base64.StdEncoding.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	if k, err := hex.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	return nil, errors.New("must be 32 bytes, base64 or hex encoded")
}

// InitSecrets loads ARTEX_SECRET_KEY, else keyDir/secret.key, else writes a new
// key. It refuses to mint a key when ciphertext already exists, then encrypts
// any plaintext rewrite-rule or hook values it still finds.
func (d *DB) InitSecrets(keyDir string) error {
	ctx := context.Background()
	return withSchemaMigrationLock(ctx, d.DB, func(conn *sql.Conn) error {
		key, err := loadSecretKey(ctx, conn, keyDir)
		if err != nil {
			return err
		}
		box, err := newSecretBox(key)
		if err != nil {
			return err
		}
		for _, c := range secretColumns {
			var v string
			err := conn.QueryRowContext(ctx, `SELECT `+c.valueExpr()+` FROM `+c.table+` WHERE `+c.valueExpr()+` LIKE 'enc:v1:%' LIMIT 1`).Scan(&v)
			if err == nil {
				_, err = box.decrypt(c.aad(), v)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("secret key check: %w", err)
			}
		}
		n, err := backfillSecrets(ctx, conn, box)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("[secrets] encrypted plaintext rewrite and hook values", "count", n)
		}
		activeSecretBox.Store(box)
		sub := deriveSubKey(key, trafficTraceDomain)
		trafficTraceKey.Store(&sub)
		return nil
	})
}

func loadSecretKey(ctx context.Context, conn *sql.Conn, keyDir string) ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv(secretKeyEnv)); v != "" {
		k, err := parseSecretKey(v)
		if err != nil {
			return nil, fmt.Errorf("%s %w", secretKeyEnv, err)
		}
		return k, nil
	}
	path := filepath.Join(keyDir, secretKeyFile)
	if data, err := os.ReadFile(path); err == nil {
		k, err := parseSecretKey(string(data))
		if err != nil {
			return nil, fmt.Errorf("secret key %s %w", path, err)
		}
		return k, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read secret key: %w", err)
	}
	var parts []string
	for _, c := range secretColumns {
		parts = append(parts, `(SELECT count(*) FROM `+c.table+` WHERE `+c.valueExpr()+` LIKE 'enc:v1:%')`)
	}
	var n int64
	if err := conn.QueryRowContext(ctx, `SELECT `+strings.Join(parts, "+")).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, fmt.Errorf("the database holds %d encrypted secret(s) but no key is available: set %s or restore %s", n, secretKeyEnv, path)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		return nil, fmt.Errorf("create secret key dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("write secret key: %w", err)
	}
	if _, err = f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err == nil {
		err = f.Close()
	}
	if err != nil {
		f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write secret key: %w", err)
	}
	return key, nil
}

func backfillSecrets(ctx context.Context, conn *sql.Conn, box *secretBox) (int, error) {
	total := 0
	for _, c := range secretColumns {
		rows, err := conn.QueryContext(ctx, `SELECT `+c.idCol()+`, `+c.col+`::text FROM `+c.table+` WHERE `+c.col+` IS NOT NULL`)
		if err != nil {
			return total, err
		}
		var ids []any
		var olds, news []string
		for rows.Next() {
			var id any
			var old string
			if err = rows.Scan(&id, &old); err != nil {
				break
			}
			if isSealed(c, old) || strings.TrimSpace(old) == "" {
				continue
			}
			nv, serr := box.seal(c, old)
			if serr != nil {
				err = fmt.Errorf("backfill %s id=%v: %w", c.aad(), id, serr)
				break
			}
			if isSealed(c, nv) {
				ids, olds, news = append(ids, id), append(olds, old), append(news, nv)
			}
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		if err != nil {
			return total, err
		}
		cast := ""
		if c.json {
			cast = "::jsonb"
		}
		for i, id := range ids {
			res, err := conn.ExecContext(ctx, `UPDATE `+c.table+` SET `+c.col+`=$1`+cast+` WHERE `+c.idCol()+`=$2 AND `+c.col+`::text=$3`, news[i], id, olds[i])
			if err != nil {
				return total, fmt.Errorf("backfill %s id=%v: %w", c.aad(), id, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				total++
			}
		}
	}
	return total, nil
}
