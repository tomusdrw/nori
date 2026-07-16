# Nori — Milestone 1: Foundation + Read-Only Dashboard — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the Go application skeleton with SQLite persistence, encrypted secrets, Docker introspection, registry digest lookup, and a live read-only htmx dashboard showing each configured service's state, running version, and latest available version.

**Architecture:** One Go binary built from small packages behind interfaces (`config`, `crypto`, `store`, `docker`, `registry`, `web`). The dashboard joins persisted services (SQLite) with live container state (Docker) and the latest registry digest. Deploy execution, automation, and auth are later milestones — this milestone is read-only and **must not be exposed publicly until M4 adds authentication**.

**Tech Stack:** Go 1.23, `modernc.org/sqlite` (pure-Go, no cgo), `github.com/docker/docker/client`, `github.com/google/go-containerregistry` (`crane`), `github.com/go-chi/chi/v5`, `github.com/a-h/templ` + htmx.

## Global Constraints

- **Go version:** 1.23. Module path: `deploybot` (bare — this is a private application, not a library).
- **No cgo:** use `modernc.org/sqlite` (driver name `"sqlite"`), never `mattn/go-sqlite3`.
- **Container grouping label:** the exact constant is `deploybot.service` (package `docker`, exported as `docker.ServiceLabel`).
- **Auto-deploy policy enum values (verbatim):** `immediate`, `manual`, `scheduled`.
- **Timestamps** are stored in SQLite as INTEGER Unix seconds and converted to/from `time.Time` (UTC) in Go — never rely on the driver's DATETIME parsing.
- **Encryption:** secret env values are encrypted at rest with AES-256-GCM. The 32-byte key comes from env var `DEPLOYBOT_KEY` (base64-encoded); it is never stored in the DB.
- **templ codegen:** run `templ generate` before any `go build`/`go test`/`go mod tidy`. The `Makefile` targets do this for you.
- Every task ends with `go build ./...` and `go test ./...` passing (via `make build` / `make test`, which generate templ first).

---

### Task 1: Project scaffolding + config loader

**Files:**
- Create: `go.mod`
- Create: `Makefile`
- Create: `.gitignore`
- Create: `cmd/deploybot/main.go`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Config{ ListenAddr string; DBPath string; EncryptionKey []byte; DockerHost string; PollInterval time.Duration }` and `config.Load() (config.Config, error)`.

- [ ] **Step 1: Create `go.mod`**

```
module deploybot

go 1.23
```

- [ ] **Step 2: Create `.gitignore`**

```
/bin/
*.db
*_templ.go
```

- [ ] **Step 3: Create `Makefile`**

```makefile
.PHONY: generate build test run tidy
generate:
	go run github.com/a-h/templ/cmd/templ@latest generate
tidy: generate
	go mod tidy
build: generate
	go build -o bin/deploybot ./cmd/deploybot
test: generate
	go test ./...
run: build
	./bin/deploybot
```

- [ ] **Step 4: Write the failing test** — `internal/config/config_test.go`

```go
package config

import (
	"encoding/base64"
	"testing"
)

func validKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("DEPLOYBOT_KEY", validKey())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if len(cfg.EncryptionKey) != 32 {
		t.Errorf("key len = %d, want 32", len(cfg.EncryptionKey))
	}
}

func TestLoad_MissingKey(t *testing.T) {
	t.Setenv("DEPLOYBOT_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing DEPLOYBOT_KEY")
	}
}

func TestLoad_BadKeyLength(t *testing.T) {
	t.Setenv("DEPLOYBOT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if _, err := Load(); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}
```

- [ ] **Step 5: Run test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL (package `config` has no `Load`).

- [ ] **Step 6: Implement `internal/config/config.go`**

```go
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"time"
)

type Config struct {
	ListenAddr    string
	DBPath        string
	EncryptionKey []byte
	DockerHost    string
	PollInterval  time.Duration
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:   getenv("DEPLOYBOT_LISTEN", ":8080"),
		DBPath:       getenv("DEPLOYBOT_DB", "deploybot.db"),
		DockerHost:   os.Getenv("DEPLOYBOT_DOCKER_HOST"),
		PollInterval: 60 * time.Second,
	}
	keyB64 := os.Getenv("DEPLOYBOT_KEY")
	if keyB64 == "" {
		return Config{}, fmt.Errorf("DEPLOYBOT_KEY is required (base64-encoded 32 bytes)")
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return Config{}, fmt.Errorf("DEPLOYBOT_KEY: %w", err)
	}
	if len(key) != 32 {
		return Config{}, fmt.Errorf("DEPLOYBOT_KEY must decode to 32 bytes, got %d", len(key))
	}
	c.EncryptionKey = key
	if v := os.Getenv("DEPLOYBOT_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("DEPLOYBOT_POLL_INTERVAL: %w", err)
		}
		c.PollInterval = d
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 7: Create minimal `cmd/deploybot/main.go`** (expanded in Task 9)

```go
package main

import (
	"log"

	"deploybot/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	_ = cfg
	log.Println("deploybot: config loaded")
}
```

- [ ] **Step 8: Run tests + build**

Run: `go test ./internal/config/` then `go build ./...`
Expected: PASS; build succeeds.

- [ ] **Step 9: Commit**

```bash
git add go.mod Makefile .gitignore cmd/deploybot/main.go internal/config/
git commit --no-gpg-sign -m "feat: project scaffolding and config loader"
```

> **Note on `--no-gpg-sign`:** this repo signs commits by default, but the agent shell can't unlock the GPG key. Every commit in this plan uses `--no-gpg-sign`; re-sign later in your own terminal if desired.

---

### Task 2: Secret encryption (`crypto`)

**Files:**
- Create: `internal/crypto/crypto.go`
- Test: `internal/crypto/crypto_test.go`

**Interfaces:**
- Produces: `crypto.Encrypt(key, plaintext []byte) ([]byte, error)` and `crypto.Decrypt(key, ciphertext []byte) ([]byte, error)`. Nonce is prepended to the ciphertext. `key` must be 32 bytes.

- [ ] **Step 1: Write the failing test** — `internal/crypto/crypto_test.go`

```go
package crypto

import (
	"bytes"
	"testing"
)

func key32() []byte { return bytes.Repeat([]byte{7}, 32) }

func TestRoundTrip(t *testing.T) {
	pt := []byte("super-secret-value")
	ct, err := Encrypt(key32(), pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(ct, pt) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := Decrypt(key32(), ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestDecrypt_Tampered(t *testing.T) {
	ct, _ := Encrypt(key32(), []byte("x"))
	ct[len(ct)-1] ^= 0xFF
	if _, err := Decrypt(key32(), ct); err == nil {
		t.Fatal("expected error on tampered ciphertext")
	}
}

func TestDecrypt_TooShort(t *testing.T) {
	if _, err := Decrypt(key32(), []byte("short")); err == nil {
		t.Fatal("expected error on short ciphertext")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/crypto/`
Expected: FAIL (no `Encrypt`).

- [ ] **Step 3: Implement `internal/crypto/crypto.go`**

```go
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

func Encrypt(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func Decrypt(key, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/crypto/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/crypto/
git commit --no-gpg-sign -m "feat: AES-GCM secret encryption"
```

---

### Task 3: Store — schema + Service repository

**Files:**
- Create: `internal/store/models.go`
- Create: `internal/store/store.go`
- Create: `internal/store/service.go`
- Test: `internal/store/service_test.go`

**Interfaces:**
- Consumes: nothing (crypto used in Task 4).
- Produces:
  - Types `store.Policy` (`PolicyImmediate`/`PolicyManual`/`PolicyScheduled`), `store.Service`, `store.EnvVar`, `store.Deployment`.
  - `store.Open(path string, key []byte) (*store.Store, error)`, `(*Store).Close() error`.
  - `(*Store).CreateService(ctx, *Service) error` (sets `ID`, `CreatedAt`, `UpdatedAt`), `(*Store).GetService(ctx, id int64) (*Service, error)` (returns `store.ErrNotFound`), `(*Store).ListServices(ctx) ([]*Service, error)`.

- [ ] **Step 1: Create `internal/store/models.go`**

```go
package store

import "time"

type Policy string

const (
	PolicyImmediate Policy = "immediate"
	PolicyManual    Policy = "manual"
	PolicyScheduled Policy = "scheduled"
)

type Service struct {
	ID           int64
	Name         string
	WatchedImage string
	Policy       Policy
	CronExpr     string
	DeployScript string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type EnvVar struct {
	ID        int64
	ServiceID int64
	Key       string
	Value     string
	IsSecret  bool
}

type Deployment struct {
	ID           int64
	ServiceID    int64
	Trigger      string
	TargetDigest string
	Status       string
	StartedAt    time.Time
	FinishedAt   *time.Time
	Log          string
}
```

- [ ] **Step 2: Create `internal/store/store.go`**

```go
package store

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	key []byte
}

const schema = `
CREATE TABLE IF NOT EXISTS service (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	watched_image TEXT NOT NULL,
	policy TEXT NOT NULL,
	cron_expr TEXT NOT NULL DEFAULT '',
	deploy_script TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS env_var (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	service_id INTEGER NOT NULL REFERENCES service(id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	value BLOB NOT NULL,
	is_secret INTEGER NOT NULL DEFAULT 0,
	UNIQUE(service_id, key)
);
CREATE TABLE IF NOT EXISTS deployment (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	service_id INTEGER NOT NULL REFERENCES service(id) ON DELETE CASCADE,
	trigger TEXT NOT NULL,
	target_digest TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	finished_at INTEGER,
	log TEXT NOT NULL DEFAULT ''
);
`

func Open(path string, key []byte) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, key: key}, nil
}

func (s *Store) Close() error { return s.db.Close() }
```

- [ ] **Step 3: Write the failing test** — `internal/store/service_test.go`

```go
package store

import (
	"context"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path, make([]byte, 32))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestServiceCRUD(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "blog", WatchedImage: "ghcr.io/me/blog:latest", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if svc.ID == 0 {
		t.Fatal("ID not set")
	}
	if svc.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not set")
	}

	got, err := st.GetService(ctx, svc.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.Name != "blog" || got.WatchedImage != "ghcr.io/me/blog:latest" || got.Policy != PolicyManual {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	list, err := st.ListServices(ctx)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1", len(list))
	}
}

func TestGetService_NotFound(t *testing.T) {
	st := testStore(t)
	if _, err := st.GetService(context.Background(), 999); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: FAIL (no `CreateService`).

- [ ] **Step 5: Implement `internal/store/service.go`**

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")

func (s *Store) CreateService(ctx context.Context, svc *Service) error {
	now := time.Now().UTC()
	svc.CreatedAt, svc.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO service (name, watched_image, policy, cron_expr, deploy_script, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?)`,
		svc.Name, svc.WatchedImage, string(svc.Policy), svc.CronExpr, svc.DeployScript,
		svc.CreatedAt.Unix(), svc.UpdatedAt.Unix())
	if err != nil {
		return err
	}
	svc.ID, err = res.LastInsertId()
	return err
}

func (s *Store) GetService(ctx context.Context, id int64) (*Service, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id,name,watched_image,policy,cron_expr,deploy_script,created_at,updated_at
		 FROM service WHERE id=?`, id)
	return scanService(row)
}

func (s *Store) ListServices(ctx context.Context) ([]*Service, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,name,watched_image,policy,cron_expr,deploy_script,created_at,updated_at
		 FROM service ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Service
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func scanService(sc rowScanner) (*Service, error) {
	var svc Service
	var policy string
	var created, updated int64
	err := sc.Scan(&svc.ID, &svc.Name, &svc.WatchedImage, &policy,
		&svc.CronExpr, &svc.DeployScript, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	svc.Policy = Policy(policy)
	svc.CreatedAt = time.Unix(created, 0).UTC()
	svc.UpdatedAt = time.Unix(updated, 0).UTC()
	return &svc, nil
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS. (First run downloads `modernc.org/sqlite`; run `go mod tidy` if needed.)

- [ ] **Step 7: Commit**

```bash
go mod tidy
git add internal/store/ go.mod go.sum
git commit --no-gpg-sign -m "feat: SQLite store with service repository"
```

---

### Task 4: Store — EnvVar repository with encryption at rest

**Files:**
- Create: `internal/store/env.go`
- Test: `internal/store/env_test.go`

**Interfaces:**
- Consumes: `crypto.Encrypt`/`crypto.Decrypt`; `Store.key` from Task 3.
- Produces: `(*Store).SetEnvVar(ctx, *EnvVar) error` (upsert by `(service_id,key)`), `(*Store).ListEnvVars(ctx, serviceID int64) ([]*EnvVar, error)` (secret values decrypted on read).

- [ ] **Step 1: Write the failing test** — `internal/store/env_test.go`

```go
package store

import (
	"context"
	"testing"
)

func TestEnvVar_SecretEncryptedAtRest(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	secret := "hunter2"
	if err := st.SetEnvVar(ctx, &EnvVar{ServiceID: svc.ID, Key: "DB_PASS", Value: secret, IsSecret: true}); err != nil {
		t.Fatalf("SetEnvVar: %v", err)
	}

	// Raw column must not equal the plaintext.
	var raw []byte
	if err := st.db.QueryRowContext(ctx, `SELECT value FROM env_var WHERE key='DB_PASS'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) == secret {
		t.Fatal("secret stored in plaintext")
	}

	// Read-back decrypts.
	vars, err := st.ListEnvVars(ctx, svc.ID)
	if err != nil {
		t.Fatalf("ListEnvVars: %v", err)
	}
	if len(vars) != 1 || vars[0].Value != secret {
		t.Fatalf("decrypt mismatch: %+v", vars)
	}
}

func TestEnvVar_NonSecretPlaintext(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "app", WatchedImage: "x", Policy: PolicyManual}
	st.CreateService(ctx, svc)
	if err := st.SetEnvVar(ctx, &EnvVar{ServiceID: svc.ID, Key: "PORT", Value: "8080"}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	st.db.QueryRowContext(ctx, `SELECT value FROM env_var WHERE key='PORT'`).Scan(&raw)
	if string(raw) != "8080" {
		t.Fatalf("non-secret should be plaintext, got %q", raw)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestEnvVar`
Expected: FAIL (no `SetEnvVar`).

- [ ] **Step 3: Implement `internal/store/env.go`**

```go
package store

import (
	"context"

	"deploybot/internal/crypto"
)

func (s *Store) SetEnvVar(ctx context.Context, ev *EnvVar) error {
	stored := []byte(ev.Value)
	if ev.IsSecret {
		enc, err := crypto.Encrypt(s.key, []byte(ev.Value))
		if err != nil {
			return err
		}
		stored = enc
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO env_var (service_id, key, value, is_secret) VALUES (?,?,?,?)
		 ON CONFLICT(service_id, key) DO UPDATE SET value=excluded.value, is_secret=excluded.is_secret`,
		ev.ServiceID, ev.Key, stored, boolToInt(ev.IsSecret))
	return err
}

func (s *Store) ListEnvVars(ctx context.Context, serviceID int64) ([]*EnvVar, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, service_id, key, value, is_secret FROM env_var WHERE service_id=? ORDER BY key`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EnvVar
	for rows.Next() {
		var ev EnvVar
		var raw []byte
		var isSecret int
		if err := rows.Scan(&ev.ID, &ev.ServiceID, &ev.Key, &raw, &isSecret); err != nil {
			return nil, err
		}
		ev.IsSecret = isSecret != 0
		if ev.IsSecret {
			dec, err := crypto.Decrypt(s.key, raw)
			if err != nil {
				return nil, err
			}
			ev.Value = string(dec)
		} else {
			ev.Value = string(raw)
		}
		out = append(out, &ev)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/env.go internal/store/env_test.go
git commit --no-gpg-sign -m "feat: encrypted env var storage"
```

---

### Task 5: Docker introspection (`docker`)

**Files:**
- Create: `internal/docker/docker.go`
- Create: `internal/docker/fake.go`
- Test: `internal/docker/fake_test.go`
- Test: `internal/docker/docker_integration_test.go`

**Interfaces:**
- Produces:
  - Const `docker.ServiceLabel = "deploybot.service"`.
  - Type `docker.Container{ ID, Name, Image, Digest, State string }` (`Digest` is the repo/manifest digest `sha256:...`; `State` is e.g. `running`, `exited`).
  - Interface `docker.Client interface { ListByService(ctx, service string) ([]Container, error) }`.
  - `docker.New(host string) (Client, error)` (empty host = SDK default / `DOCKER_HOST`).
  - `docker.Fake{ Containers map[string][]Container; Err error }` implementing `Client` (for consumer tests).

- [ ] **Step 1: Implement `internal/docker/docker.go`**

```go
package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

const ServiceLabel = "deploybot.service"

type Container struct {
	ID     string
	Name   string
	Image  string
	Digest string
	State  string
}

type Client interface {
	ListByService(ctx context.Context, service string) ([]Container, error)
}

type realClient struct{ cli *client.Client }

func New(host string) (Client, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append([]client.Opt{client.WithHost(host)}, opts...)
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, err
	}
	return &realClient{cli: cli}, nil
}

func (r *realClient) ListByService(ctx context.Context, service string) ([]Container, error) {
	f := filters.NewArgs(filters.Arg("label", fmt.Sprintf("%s=%s", ServiceLabel, service)))
	list, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list))
	for _, c := range list {
		digest := ""
		if insp, _, err := r.cli.ImageInspectWithRaw(ctx, c.ImageID); err == nil && len(insp.RepoDigests) > 0 {
			if i := strings.Index(insp.RepoDigests[0], "@"); i >= 0 {
				digest = insp.RepoDigests[0][i+1:]
			}
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, Container{ID: c.ID, Name: name, Image: c.Image, Digest: digest, State: c.State})
	}
	return out, nil
}
```

- [ ] **Step 2: Implement `internal/docker/fake.go`**

```go
package docker

import "context"

type Fake struct {
	Containers map[string][]Container
	Err        error
}

func (f *Fake) ListByService(ctx context.Context, service string) ([]Container, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Containers[service], nil
}
```

- [ ] **Step 3: Write the fake test** — `internal/docker/fake_test.go`

```go
package docker

import (
	"context"
	"testing"
)

func TestFake_ImplementsClient(t *testing.T) {
	var c Client = &Fake{Containers: map[string][]Container{
		"blog": {{Name: "blog-web", State: "running", Digest: "sha256:abc"}},
	}}
	got, err := c.ListByService(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != "running" {
		t.Fatalf("unexpected: %+v", got)
	}
}
```

- [ ] **Step 4: Write the integration test** — `internal/docker/docker_integration_test.go`

```go
//go:build integration

package docker

import (
	"context"
	"testing"
	"time"
)

// Requires a running Docker daemon. Run with: go test -tags integration ./internal/docker/
func TestListByService_Integration(t *testing.T) {
	c, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := c.ListByService(ctx, "no-such-service-xyz")
	if err != nil {
		t.Fatalf("ListByService: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0, got %d", len(got))
	}
}
```

- [ ] **Step 5: Run tests + build**

Run: `go mod tidy` then `go test ./internal/docker/` (unit only; integration is tag-gated).
Expected: PASS. Then optionally `go test -tags integration ./internal/docker/` if Docker is running.

- [ ] **Step 6: Commit**

```bash
git add internal/docker/ go.mod go.sum
git commit --no-gpg-sign -m "feat: docker container introspection by service label"
```

---

### Task 6: Registry digest lookup (`registry`)

**Files:**
- Create: `internal/registry/registry.go`
- Test: `internal/registry/registry_test.go`

**Interfaces:**
- Produces: `registry.LatestDigest(ref string, opts ...crane.Option) (string, error)` returning the manifest digest `sha256:...` for the image reference. Real callers pass no opts (public GHCR over HTTPS); tests pass `crane.Insecure`.

- [ ] **Step 1: Write the failing test** — `internal/registry/registry_test.go`

```go
package registry

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

func TestLatestDigest(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/team/app:latest"

	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, ref, crane.Insecure); err != nil {
		t.Fatalf("push: %v", err)
	}
	want, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	got, err := LatestDigest(ref, crane.Insecure)
	if err != nil {
		t.Fatalf("LatestDigest: %v", err)
	}
	if got != want.String() {
		t.Fatalf("digest = %s, want %s", got, want.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/registry/`
Expected: FAIL (no `LatestDigest`).

- [ ] **Step 3: Implement `internal/registry/registry.go`**

```go
package registry

import "github.com/google/go-containerregistry/pkg/crane"

// LatestDigest returns the manifest digest (sha256:...) for the given image
// reference. Packages are public, so no auth is configured.
func LatestDigest(ref string, opts ...crane.Option) (string, error) {
	return crane.Digest(ref, opts...)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go mod tidy` then `go test ./internal/registry/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry/ go.mod go.sum
git commit --no-gpg-sign -m "feat: registry latest-digest lookup"
```

---

### Task 7: Dashboard view helpers (pure functions)

**Files:**
- Create: `internal/web/view.go`
- Test: `internal/web/view_test.go`

**Interfaces:**
- Produces (package `web`):
  - Type `ServiceView{ Name, State, RunningVersion, LatestVersion string; UpdateAvailable bool }`.
  - `repoOf(ref string) string` — strips tag/digest, returns `host/path`.
  - `watchedDigest(cs []docker.Container, watchedImage string) string` — digest of the container whose image repo matches the watched image (`""` if none).
  - `summarizeState(cs []docker.Container) string` — `none` / `running` / `stopped` / `partial`.
  - `shortDigest(d string) string` — `sha256:9f3c…` → `9f3c1a2b` (12 hex chars) or `—` when empty.

- [ ] **Step 1: Write the failing test** — `internal/web/view_test.go`

```go
package web

import (
	"testing"

	"deploybot/internal/docker"
)

func TestRepoOf(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/me/app:latest":        "ghcr.io/me/app",
		"ghcr.io/me/app":               "ghcr.io/me/app",
		"ghcr.io/me/app@sha256:abc":    "ghcr.io/me/app",
		"localhost:5000/me/app:v1":     "localhost:5000/me/app",
	}
	for in, want := range cases {
		if got := repoOf(in); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSummarizeState(t *testing.T) {
	if summarizeState(nil) != "none" {
		t.Error("nil should be none")
	}
	all := []docker.Container{{State: "running"}, {State: "running"}}
	if summarizeState(all) != "running" {
		t.Error("all running")
	}
	none := []docker.Container{{State: "exited"}, {State: "exited"}}
	if summarizeState(none) != "stopped" {
		t.Error("all stopped")
	}
	mix := []docker.Container{{State: "running"}, {State: "exited"}}
	if summarizeState(mix) != "partial" {
		t.Error("mixed = partial")
	}
}

func TestWatchedDigest(t *testing.T) {
	cs := []docker.Container{
		{Image: "postgres:16", Digest: "sha256:db"},
		{Image: "ghcr.io/me/app:latest", Digest: "sha256:app"},
	}
	if got := watchedDigest(cs, "ghcr.io/me/app:latest"); got != "sha256:app" {
		t.Errorf("watchedDigest = %q, want sha256:app", got)
	}
	if got := watchedDigest(cs, "ghcr.io/me/other:latest"); got != "" {
		t.Errorf("watchedDigest = %q, want empty", got)
	}
}

func TestShortDigest(t *testing.T) {
	if got := shortDigest("sha256:9f3c1a2bdeadbeef"); got != "9f3c1a2bdead" {
		t.Errorf("shortDigest = %q", got)
	}
	if got := shortDigest(""); got != "—" {
		t.Errorf("empty shortDigest = %q, want dash", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/`
Expected: FAIL (no `repoOf`).

- [ ] **Step 3: Implement `internal/web/view.go`**

```go
package web

import (
	"strings"

	"deploybot/internal/docker"
)

type ServiceView struct {
	Name            string
	State           string
	RunningVersion  string
	LatestVersion   string
	UpdateAvailable bool
}

func repoOf(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon > slash {
		ref = ref[:colon]
	}
	return ref
}

func watchedDigest(cs []docker.Container, watchedImage string) string {
	want := repoOf(watchedImage)
	for _, c := range cs {
		if repoOf(c.Image) == want {
			return c.Digest
		}
	}
	return ""
}

func summarizeState(cs []docker.Container) string {
	if len(cs) == 0 {
		return "none"
	}
	running := 0
	for _, c := range cs {
		if c.State == "running" {
			running++
		}
	}
	switch {
	case running == 0:
		return "stopped"
	case running == len(cs):
		return "running"
	default:
		return "partial"
	}
}

func shortDigest(d string) string {
	if d == "" {
		return "—"
	}
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/web/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/view.go internal/web/view_test.go
git commit --no-gpg-sign -m "feat: dashboard view helpers"
```

---

### Task 8: Dashboard server + templ views

**Files:**
- Create: `internal/web/server.go`
- Create: `internal/web/dashboard.templ` (generates `dashboard_templ.go`)
- Create: `internal/web/static.go`
- Create: `internal/web/static/htmx.min.js` (downloaded)
- Test: `internal/web/dashboard_test.go`

**Interfaces:**
- Consumes: `store.ListServices`, `docker.Client.ListByService`, view helpers from Task 7.
- Produces:
  - `web.LatestDigestFunc func(ctx, image string) (string, error)`.
  - `web.NewServer(st *store.Store, dk docker.Client, latest LatestDigestFunc) *web.Server`, which implements `http.Handler`.
  - Routes: `GET /` (full page), `GET /partials/services` (table fragment, htmx self-refresh every 5s), `GET /static/*`.
  - templ components `Page(views []ServiceView)` and `ServicesTable(views []ServiceView)`.

- [ ] **Step 1: Download htmx** into `internal/web/static/htmx.min.js`

```bash
mkdir -p internal/web/static
curl -fsSL https://unpkg.com/htmx.org@2.0.3/dist/htmx.min.js -o internal/web/static/htmx.min.js
```

- [ ] **Step 2: Create `internal/web/static.go`**

```go
package web

import "embed"

//go:embed static
var staticFS embed.FS
```

- [ ] **Step 3: Create `internal/web/dashboard.templ`**

```templ
package web

templ Page(views []ServiceView) {
	<!DOCTYPE html>
	<html lang="en">
		<head>
			<meta charset="utf-8"/>
			<meta name="viewport" content="width=device-width, initial-scale=1"/>
			<title>Nori</title>
			<script src="/static/htmx.min.js"></script>
		</head>
		<body>
			<h1>Services</h1>
			@ServicesTable(views)
		</body>
	</html>
}

templ ServicesTable(views []ServiceView) {
	<table id="services" hx-get="/partials/services" hx-trigger="every 5s" hx-swap="outerHTML">
		<thead>
			<tr><th>Name</th><th>State</th><th>Running</th><th>Latest</th><th>Update</th></tr>
		</thead>
		<tbody>
			for _, v := range views {
				<tr>
					<td>{ v.Name }</td>
					<td>{ v.State }</td>
					<td>{ v.RunningVersion }</td>
					<td>{ v.LatestVersion }</td>
					<td>
						if v.UpdateAvailable {
							<span>update available</span>
						}
					</td>
				</tr>
			}
		</tbody>
	</table>
}
```

- [ ] **Step 4: Create `internal/web/server.go`**

```go
package web

import (
	"context"
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"

	"deploybot/internal/docker"
	"deploybot/internal/store"
)

type LatestDigestFunc func(ctx context.Context, image string) (string, error)

type Server struct {
	store  *store.Store
	docker docker.Client
	latest LatestDigestFunc
	router chi.Router
}

func NewServer(st *store.Store, dk docker.Client, latest LatestDigestFunc) *Server {
	s := &Server{store: st, docker: dk, latest: latest}
	r := chi.NewRouter()
	r.Get("/", s.handleDashboard)
	r.Get("/partials/services", s.handleServicesPartial)
	sub, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	s.router = r
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

func (s *Server) loadViews(ctx context.Context) ([]ServiceView, error) {
	svcs, err := s.store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]ServiceView, 0, len(svcs))
	for _, svc := range svcs {
		cs, err := s.docker.ListByService(ctx, svc.Name)
		if err != nil {
			return nil, err
		}
		v := ServiceView{
			Name:           svc.Name,
			State:          summarizeState(cs),
			RunningVersion: shortDigest(watchedDigest(cs, svc.WatchedImage)),
		}
		if latest, err := s.latest(ctx, svc.WatchedImage); err == nil {
			v.LatestVersion = shortDigest(latest)
			running := watchedDigest(cs, svc.WatchedImage)
			v.UpdateAvailable = running != "" && running != latest
		} else {
			v.LatestVersion = "—"
		}
		views = append(views, v)
	}
	return views, nil
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	views, err := s.loadViews(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = Page(views).Render(r.Context(), w)
}

func (s *Server) handleServicesPartial(w http.ResponseWriter, r *http.Request) {
	views, err := s.loadViews(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = ServicesTable(views).Render(r.Context(), w)
}
```

- [ ] **Step 5: Write the failing test** — `internal/web/dashboard_test.go`

```go
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"deploybot/internal/docker"
	"deploybot/internal/store"
)

func TestDashboard_RendersServiceWithUpdate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := &store.Service{Name: "blog", WatchedImage: "ghcr.io/me/blog:latest", Policy: store.PolicyManual}
	if err := st.CreateService(context.Background(), svc); err != nil {
		t.Fatal(err)
	}

	dk := &docker.Fake{Containers: map[string][]docker.Container{
		"blog": {{Name: "blog-web", Image: "ghcr.io/me/blog:latest", Digest: "sha256:running0000", State: "running"}},
	}}
	latest := func(ctx context.Context, image string) (string, error) { return "sha256:newer00000000", nil }

	srv := NewServer(st, dk, latest)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"blog", "running", "update available"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}
```

- [ ] **Step 6: Generate templ, run test**

Run: `make test` (runs `templ generate` then `go test ./...`) — or manually:
```bash
go run github.com/a-h/templ/cmd/templ@latest generate
go mod tidy
go test ./internal/web/
```
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/web/ go.mod go.sum
git commit --no-gpg-sign -m "feat: read-only dashboard with htmx auto-refresh"
```

---

### Task 9: Wire `main`, demo seed, end-to-end smoke test

**Files:**
- Modify: `cmd/deploybot/main.go` (replace Task 1's placeholder body)
- Create: `cmd/deploybot/seed.go`

**Interfaces:**
- Consumes: `config.Load`, `store.Open`, `docker.New`, `registry.LatestDigest`, `web.NewServer`.

- [ ] **Step 1: Replace `cmd/deploybot/main.go`**

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"deploybot/internal/config"
	"deploybot/internal/docker"
	"deploybot/internal/registry"
	"deploybot/internal/store"
	"deploybot/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.DBPath, cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	if len(os.Args) > 1 && os.Args[1] == "seed-demo" {
		if err := seedDemo(context.Background(), st); err != nil {
			log.Fatalf("seed: %v", err)
		}
		log.Println("seeded demo service")
		return
	}

	dk, err := docker.New(cfg.DockerHost)
	if err != nil {
		log.Fatalf("docker: %v", err)
	}

	latest := func(ctx context.Context, image string) (string, error) {
		return registry.LatestDigest(image)
	}

	srv := web.NewServer(st, dk, latest)
	log.Printf("listening on %s", cfg.ListenAddr)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, srv))
}
```

- [ ] **Step 2: Create `cmd/deploybot/seed.go`**

```go
package main

import (
	"context"

	"deploybot/internal/store"
)

func seedDemo(ctx context.Context, st *store.Store) error {
	return st.CreateService(ctx, &store.Service{
		Name:         "demo",
		WatchedImage: "ghcr.io/library/hello-world:latest",
		Policy:       store.PolicyManual,
	})
}
```

- [ ] **Step 3: Build**

Run: `make build`
Expected: `bin/deploybot` produced.

- [ ] **Step 4: Manual smoke test**

```bash
export DEPLOYBOT_KEY=$(head -c 32 /dev/urandom | base64)
export DEPLOYBOT_DB=/tmp/deploybot-smoke.db
rm -f "$DEPLOYBOT_DB"
./bin/deploybot seed-demo
./bin/deploybot &
sleep 1
curl -s localhost:8080/ | grep -q demo && echo "SMOKE OK" || echo "SMOKE FAIL"
curl -s localhost:8080/partials/services | grep -q "hx-trigger" && echo "PARTIAL OK"
kill %1
```
Expected: `SMOKE OK` and `PARTIAL OK`. (The `demo` row shows `none`/`stopped` since no matching container is running — that's correct.)

- [ ] **Step 5: Commit**

```bash
git add cmd/deploybot/
git commit --no-gpg-sign -m "feat: wire main with demo seed"
```

---

## Self-Review (spec coverage for M1)

- **Persistence of env + script (spec goal #1):** schema + `service`/`env_var` tables and repos exist (Tasks 3–4). Editing UI is M2. ✅ foundation.
- **Dashboard: configured services, running version, running/stopped, latest version (goal #2):** Tasks 7–8 render exactly these columns from live Docker + registry. ✅
- **Logs (#3), manual deploy + start/stop (#4), auto-deploy config (#5):** intentionally deferred to M2/M3 — no task here, tracked in the milestone roadmap. ✅ (deferred, not missed)
- **Security (auth):** deferred to M4. **This milestone must not be exposed publicly.** Recorded in the plan header. ✅
- **Placeholder scan:** no TBD/TODO; every code step shows complete code. ✅
- **Type consistency:** `docker.Client.ListByService`, `store.Service.WatchedImage`, `ServiceView` fields, and `LatestDigestFunc` signatures match across Tasks 5–9. ✅

## Milestone roadmap (next plans)

- **M2 — Service management + manual control:** service create/edit forms (env + script editors writing through Tasks 3–4 repos), `executor` package (env assembly with injected `$SERVICE`, run bash via a `CommandRunner` interface, stream to `deployment.log`, per-service lock + failure cooldown), `docker` gains `Logs`/`Start`/`Stop`, deploy/start/stop handlers, SSE log view, deployment history.
- **M3 — Automation:** `poller` (cached latest digest feeding the dashboard + immediate auto-deploy), `scheduler` (`robfig/cron`).
- **M4 — Auth + packaging:** login/session/CSRF middleware, Dockerfile for the bot, README (two-declaration contract, `deploybot.service` label requirement, CI image-stamping recommendation, Cloudflare Access).
