package protocol

import (
	"testing"
)

// stubEnum is a local string-enum type used only in this test file.
type stubEnum string

const stubFoo stubEnum = "foo"

func TestScanEnum(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		var s stubEnum
		if err := ScanEnum(&s, "bar"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s != "bar" {
			t.Fatalf("got %q, want %q", s, "bar")
		}
	})

	t.Run("bytes", func(t *testing.T) {
		var s stubEnum
		if err := ScanEnum(&s, []byte("baz")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s != "baz" {
			t.Fatalf("got %q, want %q", s, "baz")
		}
	})

	t.Run("nil", func(t *testing.T) {
		s := stubFoo
		if err := ScanEnum(&s, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s != "" {
			t.Fatalf("got %q, want empty", s)
		}
	})

	t.Run("invalid_type", func(t *testing.T) {
		var s stubEnum
		err := ScanEnum(&s, 42)
		if err == nil {
			t.Fatal("expected error for int input")
		}
	})

	t.Run("works_with_Status", func(t *testing.T) {
		var s Status
		if err := ScanEnum(&s, "dispatched"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s != StatusDispatched {
			t.Fatalf("got %q, want %q", s, StatusDispatched)
		}
	})
}

func TestValueEnum(t *testing.T) {
	t.Run("non_empty", func(t *testing.T) {
		v, err := ValueEnum(stubFoo)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != "foo" {
			t.Fatalf("got %v, want %q", v, "foo")
		}
	})

	t.Run("empty", func(t *testing.T) {
		v, err := ValueEnum(stubEnum(""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != "" {
			t.Fatalf("got %v, want empty string", v)
		}
	})

	t.Run("works_with_SwapMode", func(t *testing.T) {
		v, err := ValueEnum(SwapModeTwoRobot)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != "two_robot" {
			t.Fatalf("got %v, want %q", v, "two_robot")
		}
	})
}
