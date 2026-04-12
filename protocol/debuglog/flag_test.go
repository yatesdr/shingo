package debuglog

import (
	"reflect"
	"testing"
)

func TestParseDebugFlag_NoFlag(t *testing.T) {
	filtered, filter := ParseDebugFlag([]string{"--config", "test.yaml"})
	if !reflect.DeepEqual(filtered, []string{"--config", "test.yaml"}) {
		t.Errorf("filtered = %v, want [--config test.yaml]", filtered)
	}
	if filter != nil {
		t.Errorf("filter = %v, want nil", filter)
	}
}

func TestParseDebugFlag_BareFlag(t *testing.T) {
	filtered, filter := ParseDebugFlag([]string{"--config", "test.yaml", "--log-debug"})
	if !reflect.DeepEqual(filtered, []string{"--config", "test.yaml"}) {
		t.Errorf("filtered = %v, want [--config test.yaml]", filtered)
	}
	if filter == nil {
		t.Fatal("filter is nil, want empty slice")
	}
	if len(filter) != 0 {
		t.Errorf("filter = %v, want empty slice (all subsystems)", filter)
	}
}

func TestParseDebugFlag_ShortFlag(t *testing.T) {
	filtered, filter := ParseDebugFlag([]string{"-log-debug"})
	if len(filtered) != 0 {
		t.Errorf("filtered = %v, want empty", filtered)
	}
	if filter == nil {
		t.Fatal("filter is nil")
	}
	if len(filter) != 0 {
		t.Errorf("filter = %v, want empty slice", filter)
	}
}

func TestParseDebugFlag_WithValue(t *testing.T) {
	filtered, filter := ParseDebugFlag([]string{"--log-debug=rds,kafka"})
	if len(filtered) != 0 {
		t.Errorf("filtered = %v, want empty", filtered)
	}
	if !reflect.DeepEqual(filter, []string{"rds", "kafka"}) {
		t.Errorf("filter = %v, want [rds kafka]", filter)
	}
}

func TestParseDebugFlag_ShortWithValue(t *testing.T) {
	filtered, filter := ParseDebugFlag([]string{"-log-debug=engine"})
	if len(filtered) != 0 {
		t.Errorf("filtered = %v, want empty", filtered)
	}
	if !reflect.DeepEqual(filter, []string{"engine"}) {
		t.Errorf("filter = %v, want [engine]", filter)
	}
}

func TestParseDebugFlag_EmptyValue(t *testing.T) {
	filtered, filter := ParseDebugFlag([]string{"--log-debug="})
	if len(filtered) != 0 {
		t.Errorf("filtered = %v, want empty", filtered)
	}
	if filter == nil {
		t.Fatal("filter is nil")
	}
	if len(filter) != 0 {
		t.Errorf("filter = %v, want empty slice", filter)
	}
}

func TestParseDebugFlag_MultipleArgs(t *testing.T) {
	filtered, filter := ParseDebugFlag([]string{"--config", "foo.yaml", "--log-debug=rds", "--port", "8080"})
	if !reflect.DeepEqual(filtered, []string{"--config", "foo.yaml", "--port", "8080"}) {
		t.Errorf("filtered = %v", filtered)
	}
	if !reflect.DeepEqual(filter, []string{"rds"}) {
		t.Errorf("filter = %v, want [rds]", filter)
	}
}

func TestParseDebugFlag_LastValueWins(t *testing.T) {
	filtered, filter := ParseDebugFlag([]string{"--log-debug=rds", "--log-debug=kafka"})
	if len(filtered) != 0 {
		t.Errorf("filtered = %v, want empty", filtered)
	}
	// Last occurrence wins
	if !reflect.DeepEqual(filter, []string{"kafka"}) {
		t.Errorf("filter = %v, want [kafka]", filter)
	}
}
