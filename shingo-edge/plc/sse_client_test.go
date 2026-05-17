package plc

import (
	"io"
	"strings"
	"testing"
)

func TestSSEReader_BasicEvent(t *testing.T) {
	t.Parallel()
	input := "event: greeting\ndata: hello\n\n"
	r := NewSSEReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Event != "greeting" {
		t.Errorf("event = %q, want %q", ev.Event, "greeting")
	}
	if ev.Data != "hello" {
		t.Errorf("data = %q, want %q", ev.Data, "hello")
	}

	_, err = r.Next()
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestSSEReader_MultiLineData(t *testing.T) {
	t.Parallel()
	input := "event: multi\ndata: line1\ndata: line2\ndata: line3\n\n"
	r := NewSSEReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Data != "line1\nline2\nline3" {
		t.Errorf("data = %q, want %q", ev.Data, "line1\nline2\nline3")
	}
}

func TestSSEReader_CommentsIgnored(t *testing.T) {
	t.Parallel()
	input := ": this is a comment\nevent: test\ndata: value\n: another comment\n\n"
	r := NewSSEReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Event != "test" {
		t.Errorf("event = %q, want %q", ev.Event, "test")
	}
	if ev.Data != "value" {
		t.Errorf("data = %q, want %q", ev.Data, "value")
	}
}

func TestSSEReader_MultipleEvents(t *testing.T) {
	t.Parallel()
	input := "event: first\ndata: 1\n\nevent: second\ndata: 2\n\n"
	r := NewSSEReader(strings.NewReader(input))

	ev1, err := r.Next()
	if err != nil {
		t.Fatalf("event 1 error: %v", err)
	}
	if ev1.Event != "first" || ev1.Data != "1" {
		t.Errorf("event 1: event=%q data=%q", ev1.Event, ev1.Data)
	}

	ev2, err := r.Next()
	if err != nil {
		t.Fatalf("event 2 error: %v", err)
	}
	if ev2.Event != "second" || ev2.Data != "2" {
		t.Errorf("event 2: event=%q data=%q", ev2.Event, ev2.Data)
	}
}

func TestSSEReader_IDField(t *testing.T) {
	t.Parallel()
	input := "id: 42\nevent: test\ndata: hello\n\n"
	r := NewSSEReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.ID != "42" {
		t.Errorf("id = %q, want %q", ev.ID, "42")
	}
}

func TestSSEReader_DataOnly(t *testing.T) {
	t.Parallel()
	input := "data: just data\n\n"
	r := NewSSEReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Event != "" {
		t.Errorf("event = %q, want empty", ev.Event)
	}
	if ev.Data != "just data" {
		t.Errorf("data = %q, want %q", ev.Data, "just data")
	}
}

func TestSSEReader_NoSpaceAfterColon(t *testing.T) {
	t.Parallel()
	input := "event:nospace\ndata:value\n\n"
	r := NewSSEReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Event != "nospace" {
		t.Errorf("event = %q, want %q", ev.Event, "nospace")
	}
	if ev.Data != "value" {
		t.Errorf("data = %q, want %q", ev.Data, "value")
	}
}

func TestSSEReader_EOFWithPendingEvent(t *testing.T) {
	t.Parallel()
	// No trailing blank line — event should still be dispatched at EOF
	input := "event: last\ndata: final"
	r := NewSSEReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Event != "last" || ev.Data != "final" {
		t.Errorf("event=%q data=%q", ev.Event, ev.Data)
	}
}

func TestSSEReader_EmptyStream(t *testing.T) {
	t.Parallel()
	r := NewSSEReader(strings.NewReader(""))

	_, err := r.Next()
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestSSEReader_BlankLinesOnly(t *testing.T) {
	t.Parallel()
	r := NewSSEReader(strings.NewReader("\n\n\n"))

	_, err := r.Next()
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestSSEReader_JSONData(t *testing.T) {
	t.Parallel()
	input := "event: tag_change\ndata: {\"plc\":\"PLC1\",\"tag\":\"Counter1\",\"value\":42,\"type\":\"DINT\"}\n\n"
	r := NewSSEReader(strings.NewReader(input))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Event != "tag_change" {
		t.Errorf("event = %q, want %q", ev.Event, "tag_change")
	}
	expected := `{"plc":"PLC1","tag":"Counter1","value":42,"type":"DINT"}`
	if ev.Data != expected {
		t.Errorf("data = %q, want %q", ev.Data, expected)
	}
}
