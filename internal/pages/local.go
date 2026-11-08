package pages

// LocalPath keeps a redirect taken from a form (where to go back after a
// password or a challenge) on the site that answered it: a path, never a
// URL with a host, never a protocol-relative "//evil". Anything else is
// "/".
//
// Every control character is refused, not only CR and LF. Browsers drop
// tabs and newlines anywhere in a URL before reading it (the WHATWG URL
// parser), so "/\t/evil.example" left here as a path and arrived in the
// address bar as "//evil.example": a link to the sign-in page with that
// in ?next= sent whoever signed in to another site. The backslash goes
// for the same reason: browsers read it as a slash.
func LocalPath(s string) string {
	if s == "" || s[0] != '/' {
		return "/"
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	if len(s) > 1 && s[1] == '/' {
		return "/"
	}
	return s
}
