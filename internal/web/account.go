package web

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// gravatarOrigin is where the browser loads a Gravatar from.
const gravatarOrigin = "https://gravatar.com"

// avatar is the Gravatar to draw for email: none when Gravatar is off or
// the ID token has no address, blanks alone counting as none.
func (s *Server) avatar(email string) string {
	if !s.cfg.Gravatar || strings.TrimSpace(email) == "" {
		return ""
	}
	return gravatarURL(email)
}

// gravatarURL asks for a blank image when the address has no Gravatar, so the
// initial underneath shows through.
func gravatarURL(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return gravatarOrigin + "/avatar/" + hex.EncodeToString(sum[:]) + "?s=64&d=blank"
}

// initial is the first letter or digit of name in upper case.
func initial(name string) string {
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return string(unicode.ToUpper(r))
		}
	}
	return "?"
}
