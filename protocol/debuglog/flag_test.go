package debuglog

import (
	"reflect"
	"testing"
)

func TestParseDebugFlag(t *testing.T) {
	t.Parallel()

	// filter has tri-state semantics:
	//   nil          → flag not present
	//   []string{}   → bare flag / explicit empty value (all subsystems)
	//   [a, b]       → only listed subsystems
	cases := []struct {
		name     string
		args     []string
		filtered []string
		filter   []string
		filterEmpty bool // true means filter must be non-nil len-0 slice
	}{
		{
			name:     "no flag",
			args:     []string{"--config", "test.yaml"},
			filtered: []string{"--config", "test.yaml"},
			filter:   nil,
		},
		{
			name:        "bare flag",
			args:        []string{"--config", "test.yaml", "--log-debug"},
			filtered:    []string{"--config", "test.yaml"},
			filterEmpty: true,
		},
		{
			name:        "short flag",
			args:        []string{"-log-debug"},
			filtered:    []string{},
			filterEmpty: true,
		},
		{
			name:     "with value",
			args:     []string{"--log-debug=rds,kafka"},
			filtered: []string{},
			filter:   []string{"rds", "kafka"},
		},
		{
			name:     "short with value",
			args:     []string{"-log-debug=engine"},
			filtered: []string{},
			filter:   []string{"engine"},
		},
		{
			name:        "empty value",
			args:        []string{"--log-debug="},
			filtered:    []string{},
			filterEmpty: true,
		},
		{
			name:     "multiple args",
			args:     []string{"--config", "foo.yaml", "--log-debug=rds", "--port", "8080"},
			filtered: []string{"--config", "foo.yaml", "--port", "8080"},
			filter:   []string{"rds"},
		},
		{
			name:     "last value wins",
			args:     []string{"--log-debug=rds", "--log-debug=kafka"},
			filtered: []string{},
			filter:   []string{"kafka"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			filtered, filter := ParseDebugFlag(tc.args)

			// filtered: compare with semantic emptiness (nil == zero-len treated equal)
			if len(filtered) != len(tc.filtered) || (len(filtered) > 0 && !reflect.DeepEqual(filtered, tc.filtered)) {
				t.Errorf("filtered = %v, want %v", filtered, tc.filtered)
			}

			if tc.filterEmpty {
				if filter == nil {
					t.Fatal("filter is nil, want empty slice (all subsystems)")
				}
				if len(filter) != 0 {
					t.Errorf("filter = %v, want empty slice", filter)
				}
				return
			}

			if !reflect.DeepEqual(filter, tc.filter) {
				t.Errorf("filter = %v, want %v", filter, tc.filter)
			}
		})
	}
}
