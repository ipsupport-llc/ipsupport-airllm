package store

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// settingN decodes a {"n": <int>} settings payload for comparison —
// Postgres' jsonb round-trips the value but not the original formatting.
func settingN(t *testing.T, raw []byte) int {
	t.Helper()
	var v struct {
		N int `json:"n"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v.N
}

func TestPutSettingIfAbsentConverges(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := &Store{PG: pool}
	const name = "test_put_setting_if_absent"
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM settings WHERE name = $1`, name)
	})

	const n = 10
	results := make([][]byte, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			v, err := s.PutSettingIfAbsent(ctx, name, []byte(`{"n":`+string(rune('0'+i))+`}`))
			if err != nil {
				t.Errorf("PutSettingIfAbsent(%d): %v", i, err)
				return
			}
			results[i] = v
		}(i)
	}
	wg.Wait()

	stored, err := s.GetSetting(ctx, name)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if stored == nil {
		t.Fatal("no value persisted")
	}
	for i, r := range results {
		if string(r) != string(stored) {
			t.Errorf("caller %d converged on %s, want %s (whatever is actually stored)", i, r, stored)
		}
	}
}

func TestPutSettingIfAbsentKeepsFirstValue(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := &Store{PG: pool}
	const name = "test_put_setting_if_absent_keeps_first"
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM settings WHERE name = $1`, name)
	})

	first, err := s.PutSettingIfAbsent(ctx, name, []byte(`{"n":1}`))
	if err != nil {
		t.Fatalf("first PutSettingIfAbsent: %v", err)
	}
	if n := settingN(t, first); n != 1 {
		t.Fatalf("first call got n=%d, want 1", n)
	}

	second, err := s.PutSettingIfAbsent(ctx, name, []byte(`{"n":2}`))
	if err != nil {
		t.Fatalf("second PutSettingIfAbsent: %v", err)
	}
	if n := settingN(t, second); n != 1 {
		t.Errorf("second call got n=%d, want the first-written value 1", n)
	}
}
