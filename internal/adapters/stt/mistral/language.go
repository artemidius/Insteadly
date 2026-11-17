package mistral

import (
	"errors"
	"strings"
)

// normalizeLanguage maps a BCP-47 tag to the two-letter ISO 639-1 primary subtag Mistral
// requires. Empty stays empty (provider default); a malformed or non-alpha-2 primary
// subtag is rejected before any dial.
func normalizeLanguage(lang string) (string, error) {
	lang = strings.TrimSpace(lang)
	if lang == "" {
		return "", nil
	}
	primary, _, _ := strings.Cut(strings.ToLower(lang), "-")
	if len(primary) != 2 || primary[0] < 'a' || primary[0] > 'z' || primary[1] < 'a' || primary[1] > 'z' {
		return "", errors.New("mistral stt: invalid language: want a two-letter ISO 639-1 primary subtag")
	}
	return primary, nil
}
