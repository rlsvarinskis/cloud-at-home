package pg_test

import (
	"context"
	"os"
	"testing"
	"testing/fstest"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/pg"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/pg/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

func TestMigrateAppliesInOrderOnceAndIsConcurrencySafe(t *testing.T) {
	ctx := context.Background()
	pool, err := pg.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	fsys := fstest.MapFS{
		"m/002_second.up.sql": {Data: []byte(`INSERT INTO t VALUES ('two');`)},
		"m/001_first.up.sql":  {Data: []byte(`CREATE TABLE t (v text);  INSERT INTO t VALUES ('one');`)},
		"m/README.md":         {Data: []byte("ignored")},
	}
	errc := make(chan error, 4)
	for i := 0; i < 4; i++ { // four "replicas" starting together
		go func() { errc <- pg.Migrate(ctx, pool, fsys, "m", "schema_test") }()
	}
	for i := 0; i < 4; i++ {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	rows, _ := pool.Query(ctx, `SELECT v FROM t ORDER BY ctid`)
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		got = append(got, v)
	}
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("migrations ran %v, want [one two] exactly once each", got)
	}
}

func TestMigrateFailsOnBadSQLAndRollsBack(t *testing.T) {
	ctx := context.Background()
	pool, err := pg.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	fsys := fstest.MapFS{"m/001_bad.up.sql": {Data: []byte(`CREATE TABLE ok (a int); SELECT nonsense_column FROM nowhere;`)}}
	if err := pg.Migrate(ctx, pool, fsys, "m", "schema_test"); err == nil {
		t.Fatal("expected an error")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name='ok'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a failed migration must leave nothing behind (tables=%d err=%v)", n, err)
	}
}
