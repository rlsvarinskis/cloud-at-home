// Package pgtest gives tests a fresh, empty Postgres database.
//
// If HOMECLOUD_TEST_POSTGRES is set it is used as the admin DSN of an existing server. Otherwise a real
// Postgres is started once per test process with github.com/fergusstrange/embedded-postgres (binaries are
// downloaded on first use and cached). Set HOMECLOUD_TEST_POSTGRES=skip to skip Postgres tests entirely.
package pgtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embedded "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
)

var (
	once       sync.Once
	adminDSN   string
	startErr   error
	counter    atomic.Int64
	registered atomic.Bool
	server     *embedded.EmbeddedPostgres
	baseDir    string
)

// Run wraps a package's TestMain so the embedded server is stopped when its tests finish:
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }
//
// DSN refuses to start a server without it, because an unstopped server would outlive the test binary.
func Run(m *testing.M) int {
	registered.Store(true)
	code := m.Run()
	if server != nil {
		_ = server.Stop()
	}
	if baseDir != "" {
		_ = os.RemoveAll(baseDir)
	}
	return code
}

func start() {
	if v := os.Getenv("HOMECLOUD_TEST_POSTGRES"); v != "" {
		adminDSN = v
		return
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		startErr = err
		return
	}
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	base, err := os.MkdirTemp("", "homecloud-pg-")
	if err != nil {
		startErr = err
		return
	}
	baseDir = base
	cache := filepath.Join(os.TempDir(), "homecloud-pg-cache") // binaries survive between runs
	server = embedded.NewDatabase(embedded.DefaultConfig().
		Port(port).RuntimePath(filepath.Join(base, "run")).DataPath(filepath.Join(base, "data")).
		BinariesPath(cache).StartTimeout(90 * time.Second))
	if err := server.Start(); err != nil {
		startErr = fmt.Errorf("start embedded postgres: %w", err)
		return
	}
	adminDSN = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
}

// DSN returns the DSN of a new empty database, dropped when the test ends.
func DSN(t testing.TB) string {
	t.Helper()
	if os.Getenv("HOMECLOUD_TEST_POSTGRES") == "skip" {
		t.Skip("Postgres tests disabled (HOMECLOUD_TEST_POSTGRES=skip)")
	}
	if os.Getenv("HOMECLOUD_TEST_POSTGRES") == "" && !registered.Load() {
		t.Fatal("pgtest: add `func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }` to this package's tests")
	}
	once.Do(start)
	if startErr != nil {
		t.Skipf("no Postgres available: %v", startErr)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("t_%d_%d", os.Getpid(), counter.Add(1))
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, adminDSN)
		if err != nil {
			return
		}
		defer c.Close(ctx)
		_, _ = c.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	if i := strings.Index(adminDSN, "/postgres?"); i >= 0 {
		return adminDSN[:i] + "/" + name + adminDSN[i+len("/postgres"):]
	}
	return strings.TrimSuffix(adminDSN, "/postgres") + "/" + name
}
