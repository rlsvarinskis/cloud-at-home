package e2e_test

import (
	"os"
	"testing"

	"example.com/homecloud/internal/pg/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }
