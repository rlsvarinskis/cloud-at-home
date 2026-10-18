package e2e_test

import (
	"os"
	"testing"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/pg/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }
