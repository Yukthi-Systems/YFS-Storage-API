// Package token issues and verifies the short-lived sessions that
// authorize every upload, download, stream, and WOPI request. Every
// token is an AES-GCM sealed value binding the session to its
// file_id+file_version (rather than an unrelated random ID), cached in
// Redis under "session:<token>". The token's random nonce already makes
// it unique, so the identity sealed inside it isn't needed to build the
// Redis key — it's there so VerifyToken can cross-check the token
// wasn't issued for a different file/version than the claims it maps
// to. Binding on file_id+file_version (not file_id alone) keeps
// sessions for two versions of the same file fully independent, e.g. a
// scan or bulk-revoke keyed on identity won't conflate them. Redis is
// still the source of truth for the session's claims: verifying a token
// means fetching its claims from Redis, and revoking one means deleting
// them. The Storage API is the sole owner of both the encryption key
// and the Redis session store — the Rust API only ever receives opaque
// tokens back and relays them to clients.
package token

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/models"
	"github.com/redis/go-redis/v9"
)

// ErrInvalidToken is returned by VerifyToken for any unknown, expired,
// tampered, or wrong-action token, and by RevokeToken for an unknown
// token.
var ErrInvalidToken = errors.New("token: invalid or expired token")

// keyPrefix namespaces session keys within the shared Redis keyspace.
const keyPrefix = "session:"

// sessionKey builds the Redis key for a session: "session:<token>".
func sessionKey(tok string) string {
	return keyPrefix + tok
}

// identityOf returns the value a token's claims are sealed around:
// file_id and file_version together, so sessions for two versions of
// the same file never share an identity.
func identityOf(c models.Claims) string {
	return c.FileID + ":" + c.Version
}

// Issuer mints and verifies Storage API sessions against Redis.
type Issuer struct {
	rdb *redis.Client
	gcm cipher.AEAD
}

// Config carries the dependencies used to construct an Issuer.
type Config struct {
	Redis *redis.Client
	// EncryptionKey is a base64-encoded 32-byte (AES-256) key used to
	// seal every issued token around its file_id. Generate with e.g.
	// `openssl rand -base64 32`.
	EncryptionKey string
}

// NewIssuer builds an Issuer from cfg. The Redis client and encryption
// key must both be valid.
func NewIssuer(cfg Config) (*Issuer, error) {
	if cfg.Redis == nil {
		return nil, errors.New("token: redis client must not be nil")
	}
	key, err := decodeEncryptionKey(cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("token: constructing cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("token: constructing gcm: %w", err)
	}
	return &Issuer{rdb: cfg.Redis, gcm: gcm}, nil
}

func decodeEncryptionKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("token: encryption key must not be empty")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("token: encryption key must be base64-encoded: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("token: encryption key must decode to 32 bytes (AES-256), got %d", len(key))
	}
	return key, nil
}

// GenerateUploadToken issues a session, valid for ttl, authorizing a
// direct upload for the given claims (file id, upload id, max size,
// content type, ...).
func (i *Issuer) GenerateUploadToken(ctx context.Context, c models.Claims, ttl time.Duration) (string, time.Time, error) {
	return i.create(ctx, models.ActionUpload, ttl, c)
}

// GenerateDownloadToken issues a session, valid for ttl, authorizing a
// single file download.
func (i *Issuer) GenerateDownloadToken(ctx context.Context, c models.Claims, ttl time.Duration) (string, time.Time, error) {
	return i.create(ctx, models.ActionDownload, ttl, c)
}

// GenerateStreamToken issues a session, valid for ttl, authorizing
// range-based media streaming of a single file.
func (i *Issuer) GenerateStreamToken(ctx context.Context, c models.Claims, ttl time.Duration) (string, time.Time, error) {
	return i.create(ctx, models.ActionStream, ttl, c)
}

// GenerateWOPIToken issues a session, valid for ttl, authorizing
// Collabora Online's access_token against the WOPI host endpoints for a
// single file.
func (i *Issuer) GenerateWOPIToken(ctx context.Context, c models.Claims, ttl time.Duration) (string, time.Time, error) {
	return i.create(ctx, models.ActionWOPI, ttl, c)
}

// create mints a session sealed around c's file_id+file_version and
// caches c in Redis under session:<token>.
func (i *Issuer) create(ctx context.Context, action models.TokenAction, ttl time.Duration, c models.Claims) (string, time.Time, error) {
	if ttl <= 0 {
		return "", time.Time{}, errors.New("token: ttl must be positive")
	}
	if c.FileID == "" {
		return "", time.Time{}, errors.New("token: file_id must not be empty")
	}

	c.Action = action
	payload, err := json.Marshal(c)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token: encoding claims: %w", err)
	}

	tok, err := i.seal(identityOf(c))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token: sealing token: %w", err)
	}

	expiresAt := time.Now().Add(ttl)
	if err := i.rdb.Set(ctx, sessionKey(tok), payload, ttl).Err(); err != nil {
		return "", time.Time{}, fmt.Errorf("token: storing session: %w", err)
	}
	return tok, expiresAt, nil
}

// seal produces an opaque token binding a session to identity (a
// file_id, or a batch_id for upload sessions): an AES-GCM sealed value
// (random nonce + identity), base64url-encoded. The random nonce keeps
// repeat tokens for the same identity distinct and unguessable, while
// sealing (rather than a bare random ID) ties the token itself to what
// it authorizes.
func (i *Issuer) seal(identity string) (string, error) {
	nonce := make([]byte, i.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := i.gcm.Seal(nonce, nonce, []byte(identity), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// open reverses seal, returning the identity a token was bound to. It
// fails for any token not sealed by this Issuer's key, including one
// that has been tampered with.
func (i *Issuer) open(tokenStr string) (string, error) {
	data, err := base64.RawURLEncoding.DecodeString(tokenStr)
	if err != nil {
		return "", ErrInvalidToken
	}
	nonceSize := i.gcm.NonceSize()
	if len(data) < nonceSize {
		return "", ErrInvalidToken
	}
	nonce, sealed := data[:nonceSize], data[nonceSize:]
	plain, err := i.gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", ErrInvalidToken
	}
	return string(plain), nil
}

// VerifyToken checks tokenStr is a validly sealed token, then looks it
// up in Redis, ensuring the session exists, is not expired, authorizes
// exactly wantAction, and is still bound to the same file_id+file_version
// it was sealed with. On success it returns the embedded domain claims.
func (i *Issuer) VerifyToken(ctx context.Context, tokenStr string, wantAction models.TokenAction) (*models.Claims, error) {
	if tokenStr == "" {
		return nil, ErrInvalidToken
	}

	identity, err := i.open(tokenStr)
	if err != nil {
		return nil, err
	}

	payload, err := i.rdb.Get(ctx, sessionKey(tokenStr)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrInvalidToken
		}
		return nil, fmt.Errorf("token: fetching session: %w", err)
	}

	var c models.Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("token: decoding claims: %w", err)
	}
	if c.Action != wantAction || identityOf(c) != identity {
		return nil, ErrInvalidToken
	}
	return &c, nil
}

// RevokeToken deletes tokenStr's session from Redis, if any, so it can
// no longer be used to authorize a request. It reports ErrInvalidToken
// if the token does not correspond to a live session — including one
// that isn't validly sealed by this Issuer's key at all.
func (i *Issuer) RevokeToken(ctx context.Context, tokenStr string) error {
	if tokenStr == "" {
		return ErrInvalidToken
	}
	if _, err := i.open(tokenStr); err != nil {
		return ErrInvalidToken
	}
	n, err := i.rdb.Del(ctx, sessionKey(tokenStr)).Result()
	if err != nil {
		return fmt.Errorf("token: revoking session: %w", err)
	}
	if n == 0 {
		return ErrInvalidToken
	}
	return nil
}
