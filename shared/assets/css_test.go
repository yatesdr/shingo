package assets

import (
	"bytes"
	"testing"
)

// The stripper's cases: what goes, what stays, and the line count kept.
func TestStripCSSComments(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"a whole-line comment leaves an empty line",
			"/* head */\na{}\n", "\na{}\n"},
		{"an indented whole-line comment leaves an empty line",
			"a{\n  /* why */\n  b: 1;\n}\n", "a{\n\n  b: 1;\n}\n"},
		{"a trailing comment takes its leading space",
			"a { b: 1; /* note */\n}\n", "a { b: 1;\n}\n"},
		{"a multi-line comment keeps its line breaks",
			"/* one\n two\n three */\na{}\n", "\n\n\na{}\n"},
		{"a multi-line comment after code keeps the code's line",
			"a{} /* one\n two */ b{}\n", "a{}\n b{}\n"},
		{"between two names, an empty comment keeps them two tokens",
			"a/* x */b", "a/**/b"},
		{"between two names across lines, the empty comment keeps the breaks",
			"#a/* x\ny */.b{}", "#a/*\n*/.b{}"},
		{"next to a space, the comment goes",
			"a /* x */b c/* y */ d", "a b c d"},
		{"consecutive comments are one",
			"a/* x *//* y */ b", "a b"},
		{"consecutive comments between names are one empty comment",
			"a/* x *//* y */b", "a/**/b"},
		{"a double-quoted string is copied",
			`a{content:"/* not a comment */"}`, `a{content:"/* not a comment */"}`},
		{"a single-quoted string with an escaped quote is copied",
			`a{content:'it\'s /* kept */'}`, `a{content:'it\'s /* kept */'}`},
		{"an unquoted url() is copied",
			"a{b:url(http://x/*/y.png)}", "a{b:url(http://x/*/y.png)}"},
		{"an unquoted URL() in capitals is copied",
			"a{b:URL(/*x)}", "a{b:URL(/*x)}"},
		{"a quoted url() is a string",
			`a{b:url("/*x")} /* c */`, `a{b:url("/*x")}`},
		{"a name ending in url is not url()",
			"a{b:myurl(1) /* c */}", "a{b:myurl(1) }"},
		{"a backslash escape is copied",
			`.a\/*b{}`, `.a\/*b{}`},
		{"an unclosed comment runs to the end",
			"a{}\n/* open\n", "a{}\n\n"},
		{"an escaped space at a line end is kept",
			"a\\ /* c */\n", "a\\ \n"},
		{"CRLF line breaks inside a comment are kept",
			"/* a\r\nb */\r\nx{}", "\r\n\r\nx{}"},
		{"no comment, no change",
			":root { --a: 1px; }\n", ":root { --a: 1px; }\n"},
		{"empty", "", ""},
	} {
		got := string(StripCSSComments([]byte(c.in)))
		if got != c.want {
			t.Errorf("%s:\n  in   %q\n  got  %q\n  want %q", c.name, c.in, got, c.want)
		}
		if lines(got) != lines(c.in) {
			t.Errorf("%s: %d lines from %d", c.name, lines(got), lines(c.in))
		}
		if again := string(StripCSSComments([]byte(got))); again != got {
			t.Errorf("%s: not idempotent: %q → %q", c.name, got, again)
		}
	}
}

func lines(s string) int { return bytes.Count([]byte(s), []byte("\n")) }
