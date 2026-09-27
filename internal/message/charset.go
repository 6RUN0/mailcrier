package message

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
)

// asciiCharsets are the names of US-ASCII. cron and mailx under the C
// locale declare "ANSI_X3.4-1968", the name nl_langinfo returns there, for
// output that is often UTF-8.
var asciiCharsets = map[string]bool{
	"us-ascii": true, "ascii": true, "ansi_x3.4-1968": true, "ansi_x3.4-1986": true,
	"iso646-us": true, "iso_646.irv:1991": true, "iso-ir-6": true, "us": true,
	"ibm367": true, "cp367": true, "csascii": true,
}

// decodeText returns data as valid UTF-8, given the charset its
// Content-Type declares:
//   - UTF-8, or ASCII, or a charset without a decoder, when data is valid
//     UTF-8: data as it is, since that is what such output nearly always is;
//   - no charset or ASCII, and data not valid UTF-8: windows-1252, as a
//     browser reads undeclared 8-bit text;
//   - a known charset: its decoder, windows-1252 for ISO-8859-1 as in
//     WHATWG, so that bytes 0x80-0x9F become the quotes and dashes their
//     writer meant instead of invisible control characters;
//
// and in every case a byte sequence that stays invalid becomes U+FFFD.
func decodeText(data []byte, charset string) string {
	name := strings.ToLower(strings.TrimSpace(charset))
	if name == "" || asciiCharsets[name] {
		if utf8.Valid(data) {
			return string(data)
		}
		return decodeWindows1252(data)
	}
	var enc encoding.Encoding
	if name != "utf-8" && name != "utf8" {
		enc = lookupCharset(name)
	}
	if enc == nil {
		if utf8.Valid(data) {
			return string(data)
		}
		return strings.ToValidUTF8(string(data), string(utf8.RuneError))
	}
	decoded, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		return strings.ToValidUTF8(string(data), string(utf8.RuneError))
	}
	return strings.ToValidUTF8(string(decoded), string(utf8.RuneError))
}

// lookupCharset returns the decoder of an IANA or WHATWG charset name, or
// nil when there is none. WHATWG maps charsets that browsers refuse to
// decode, ISO-2022-KR among them, to its replacement encoding, which turns
// the whole input into one U+FFFD; that counts as no decoder.
func lookupCharset(name string) encoding.Encoding {
	enc, err := ianaindex.MIME.Encoding(name)
	if err != nil || enc == nil {
		enc, err = htmlindex.Get(name)
	}
	switch {
	case err != nil || enc == nil || enc == encoding.Replacement:
		return nil
	case enc == charmap.ISO8859_1:
		return charmap.Windows1252
	default:
		return enc
	}
}

// decodeWindows1252 reads data as windows-1252; the five bytes it leaves
// undefined become U+FFFD.
func decodeWindows1252(data []byte) string {
	decoded, err := charmap.Windows1252.NewDecoder().Bytes(data)
	if err != nil {
		return strings.ToValidUTF8(string(data), string(utf8.RuneError))
	}
	return string(decoded)
}

// validText returns s when it is valid UTF-8, else s read as
// windows-1252: an 8-bit header value without an encoded word carries no
// charset.
func validText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return decodeWindows1252([]byte(s))
}
