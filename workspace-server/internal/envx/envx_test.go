package envx

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	const key = "__envx_test_duration"
	t.Run("unset uses default", func(t *testing.T) {
		t.Setenv(key, "")
		if got := Duration(key, 42*time.Second); got != 42*time.Second {
			t.Errorf("want default, got %v", got)
		}
	})
	t.Run("valid value parsed", func(t *testing.T) {
		t.Setenv(key, "30s")
		if got := Duration(key, time.Second); got != 30*time.Second {
			t.Errorf("want 30s, got %v", got)
		}
	})
	t.Run("unparseable falls back", func(t *testing.T) {
		t.Setenv(key, "not-a-duration")
		if got := Duration(key, 5*time.Second); got != 5*time.Second {
			t.Errorf("want default, got %v", got)
		}
	})
	t.Run("zero falls back", func(t *testing.T) {
		t.Setenv(key, "0")
		if got := Duration(key, 5*time.Second); got != 5*time.Second {
			t.Errorf("want default, got %v", got)
		}
	})
	t.Run("negative falls back", func(t *testing.T) {
		t.Setenv(key, "-1h")
		if got := Duration(key, 5*time.Second); got != 5*time.Second {
			t.Errorf("want default, got %v", got)
		}
	})
}

func TestInt64(t *testing.T) {
	const key = "__envx_test_int64"
	t.Run("unset uses default", func(t *testing.T) {
		t.Setenv(key, "")
		if got := Int64(key, 99); got != 99 {
			t.Errorf("want default, got %d", got)
		}
	})
	t.Run("valid value parsed", func(t *testing.T) {
		t.Setenv(key, "123")
		if got := Int64(key, 1); got != 123 {
			t.Errorf("want 123, got %d", got)
		}
	})
	t.Run("unparseable falls back", func(t *testing.T) {
		t.Setenv(key, "nope")
		if got := Int64(key, 5); got != 5 {
			t.Errorf("want default, got %d", got)
		}
	})
	t.Run("zero falls back", func(t *testing.T) {
		t.Setenv(key, "0")
		if got := Int64(key, 5); got != 5 {
			t.Errorf("want default, got %d", got)
		}
	})
	t.Run("negative falls back", func(t *testing.T) {
		t.Setenv(key, "-10")
		if got := Int64(key, 5); got != 5 {
			t.Errorf("want default, got %d", got)
		}
	})
}
