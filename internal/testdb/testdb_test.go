package testdb

import "testing"

func TestDSNForIgnoresDatabaseOverrides(t *testing.T) {
	for _, admin := range []string{
		"postgres://u:p@127.0.0.1:5432/dev?sslmode=disable",
		"postgres://u:p@127.0.0.1:5432/dev?dbname=dev&sslmode=disable",
		"postgres://u:p@127.0.0.1:5432/dev?database=dev",
	} {
		d, err := dsnFor(admin, "airllm_test_x_00000000")
		if err != nil {
			t.Fatalf("%s: %v", admin, err)
		}
		if d == "" {
			t.Fatalf("%s: empty DSN", admin)
		}
	}
}

func TestDSNForRejectsKeywordDSN(t *testing.T) {
	if _, err := dsnFor("host=db2 dbname=app user=airllm", "airllm_test_x_00000000"); err == nil {
		t.Fatal("keyword DSN accepted; it must be rejected before any DDL runs")
	}
}
