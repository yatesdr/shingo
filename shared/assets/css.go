package assets

// StripCSSComments returns src with its /* */ comments taken out, for serving.
// Comments are most of what a stylesheet sends: tokens.css is mostly prose
// about each token (round 2: 33.7 → 10.2 KB gzipped over the station's four
// stylesheets).
//
// LINES ARE KEPT: every line break inside a comment is written out again, so
// a line number DevTools shows on a plant is the line in the repo.
//
// TOKENS ARE KEPT. A comment between two non-blank characters is the only
// thing separating two tokens there (`a/**/b` is two identifiers, `ab` is
// one), so it is written as an empty comment, its line breaks inside. A
// comment next to whitespace goes entirely, and with it the spaces it leaves
// at the end of a line. Consecutive comments count as one.
//
// WHAT IS NOT A COMMENT is copied as it is: a quoted string, an unquoted
// url(...) (where `/*` is part of the URL), and a backslash escape.
//
// It is idempotent: stripping its output changes nothing.
func StripCSSComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '/' && i+1 < n && src[i+1] == '*':
			// The run of comments starting here, and the line breaks in it.
			var breaks []byte
			j := i
			for j+1 < n && src[j] == '/' && src[j+1] == '*' {
				k := j + 2
				for k < n && !(src[k] == '*' && k+1 < n && src[k+1] == '/') {
					if isCSSNewline(src[k]) {
						breaks = append(breaks, src[k])
					}
					k++
				}
				j = min(k+2, n) // an unclosed comment runs to the end
			}
			blankBefore := len(out) == 0 || isCSSSpace(out[len(out)-1])
			blankAfter := j >= n || isCSSSpace(src[j])
			if !blankBefore && !blankAfter {
				out = append(out, '/', '*')
				out = append(out, breaks...)
				out = append(out, '*', '/')
			} else {
				if len(breaks) > 0 || j >= n || isCSSNewline(src[j]) {
					out = trimLineEnd(out)
				}
				out = append(out, breaks...)
			}
			i = j
		case c == '"' || c == '\'':
			j := i + 1
			for j < n && src[j] != c && !isCSSNewline(src[j]) {
				if src[j] == '\\' && j+1 < n {
					j++ // an escaped newline continues the string
				}
				j++
			}
			if j < n && src[j] == c {
				j++
			}
			out = append(out, src[i:j]...)
			i = j
		case c == '\\':
			j := min(i+2, n)
			out = append(out, src[i:j]...)
			i = j
		case (c == 'u' || c == 'U') && isUnquotedURL(src, i):
			j := i + 4
			for j < n && src[j] != ')' {
				if src[j] == '\\' && j+1 < n {
					j++
				}
				j++
			}
			j = min(j+1, n)
			out = append(out, src[i:j]...)
			i = j
		default:
			out = append(out, c)
			i++
		}
	}
	return out
}

// isUnquotedURL: src[i:] starts url( as a token, not as the tail of a longer
// name, and its argument is not quoted (a quoted one is a string, copied by
// the string case).
func isUnquotedURL(src []byte, i int) bool {
	if i+4 > len(src) || !(src[i+1]|0x20 == 'r' && src[i+2]|0x20 == 'l' && src[i+3] == '(') {
		return false
	}
	if i > 0 && isCSSNameByte(src[i-1]) {
		return false
	}
	j := i + 4
	for j < len(src) && isCSSSpace(src[j]) {
		j++
	}
	return j >= len(src) || (src[j] != '"' && src[j] != '\'')
}

// trimLineEnd drops the spaces and tabs a removed comment left at the end of
// a line, but not an escaped one (`\ ` is part of a name).
func trimLineEnd(out []byte) []byte {
	k := len(out)
	for k > 0 && (out[k-1] == ' ' || out[k-1] == '\t') {
		bs := 0
		for m := k - 2; m >= 0 && out[m] == '\\'; m-- {
			bs++
		}
		if bs%2 == 1 {
			break
		}
		k--
	}
	return out[:k]
}

func isCSSNewline(c byte) bool { return c == '\n' || c == '\r' || c == '\f' }

func isCSSSpace(c byte) bool { return c == ' ' || c == '\t' || isCSSNewline(c) }

func isCSSNameByte(c byte) bool {
	return c == '-' || c == '_' || c >= 0x80 ||
		c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
