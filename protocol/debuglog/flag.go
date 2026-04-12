package debuglog

import "strings"

// ParseDebugFlag scans args for --log-debug / -log-debug / --log-debug=FILTER
// flags, strips them from the argument list, and returns:
//   - filtered: the remaining args with debug flags removed
//   - fileFilter: nil if no --log-debug found; empty slice for all subsystems;
//     non-empty for specific subsystems
//
// This handles the custom flag format because Go's standard flag package
// always requires a value argument, but we want bare --log-debug to mean
// "all subsystems".
func ParseDebugFlag(args []string) (filtered []string, fileFilter []string) {
	var filter []string
	found := false
	for _, arg := range args {
		switch {
		case arg == "--log-debug" || arg == "-log-debug":
			found = true
			filter = []string{}
		case strings.HasPrefix(arg, "--log-debug=") || strings.HasPrefix(arg, "-log-debug="):
			found = true
			val := arg[strings.Index(arg, "=")+1:]
			if val == "" {
				filter = []string{}
			} else {
				filter = strings.Split(val, ",")
			}
		default:
			filtered = append(filtered, arg)
		}
	}
	if !found {
		return filtered, nil
	}
	return filtered, filter
}
