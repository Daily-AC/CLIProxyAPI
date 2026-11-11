package helps

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// zeroWidthSpace is the Unicode zero-width space character used for obfuscation.
const zeroWidthSpace = "\u200B"

// combiningGraphemeJoiner (U+034F) obfuscates Antigravity system instructions: since
// 2026-11-10 Antigravity strips U+200B before matching blocked phrases (upstream #7433).
const combiningGraphemeJoiner = "\u034F"

// SensitiveWordMatcher holds the compiled regex for matching sensitive words.
type SensitiveWordMatcher struct {
	regex *regexp.Regexp
	// obfuscate rewrites one match; nil means obfuscateWord (U+200B).
	obfuscate func(string) string
}

// BuildAntigravitySensitiveWordMatcher is BuildSensitiveWordMatcher with per-subword
// U+034F obfuscation, ported from upstream 9a7e1e6b without its literal replacements.
func BuildAntigravitySensitiveWordMatcher(words []string) *SensitiveWordMatcher {
	m := BuildSensitiveWordMatcher(words)
	if m != nil {
		m.obfuscate = obfuscateAntigravityPhrase
	}
	return m
}

// BuildSensitiveWordMatcher compiles a regex from the word list.
// Words are sorted by length (longest first) for proper matching.
func BuildSensitiveWordMatcher(words []string) *SensitiveWordMatcher {
	if len(words) == 0 {
		return nil
	}

	// Filter and normalize words
	var validWords []string
	for _, w := range words {
		w = strings.TrimSpace(w)
		if utf8.RuneCountInString(w) >= 2 && !strings.Contains(w, zeroWidthSpace) {
			validWords = append(validWords, w)
		}
	}

	if len(validWords) == 0 {
		return nil
	}

	// Sort by length (longest first) for proper matching
	sort.Slice(validWords, func(i, j int) bool {
		return len(validWords[i]) > len(validWords[j])
	})

	// Escape and join
	escaped := make([]string, len(validWords))
	for i, w := range validWords {
		escaped[i] = regexp.QuoteMeta(w)
	}

	pattern := "(?i)" + strings.Join(escaped, "|")
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}

	return &SensitiveWordMatcher{regex: re}
}

// obfuscateWord inserts a zero-width space after the first grapheme.
func obfuscateWord(word string) string {
	if strings.Contains(word, zeroWidthSpace) {
		return word
	}

	// Get first rune
	r, size := utf8.DecodeRuneInString(word)
	if r == utf8.RuneError || size >= len(word) {
		return word
	}

	return string(r) + zeroWidthSpace + word[size:]
}

func isAntigravitySubwordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\u034F' || r == '\u200B'
}

func obfuscateSingleSubwordCGJ(subword string) (string, bool) {
	if strings.Contains(subword, combiningGraphemeJoiner) {
		return subword, true
	}
	r, size := utf8.DecodeRuneInString(subword)
	if r == utf8.RuneError || size >= len(subword) {
		return subword, false
	}
	return string(r) + combiningGraphemeJoiner + subword[size:], true
}

// obfuscateAntigravityPhrase inserts U+034F after the first rune of every
// alphanumeric subword (>= 2 runes) within phrase, preserving punctuation and whitespace.
func obfuscateAntigravityPhrase(phrase string) string {
	if phrase == "" || strings.Contains(phrase, combiningGraphemeJoiner) {
		return phrase
	}

	var b strings.Builder
	b.Grow(len(phrase) + len(combiningGraphemeJoiner)*2)
	subwordStart := -1
	inserted := false

	for i, r := range phrase {
		if isAntigravitySubwordRune(r) {
			if subwordStart < 0 {
				subwordStart = i
			}
			continue
		}
		if subwordStart >= 0 {
			obf, did := obfuscateSingleSubwordCGJ(phrase[subwordStart:i])
			b.WriteString(obf)
			if did {
				inserted = true
			}
			subwordStart = -1
		}
		b.WriteRune(r)
	}
	if subwordStart >= 0 {
		obf, did := obfuscateSingleSubwordCGJ(phrase[subwordStart:])
		b.WriteString(obf)
		if did {
			inserted = true
		}
	}

	if inserted {
		return b.String()
	}
	r, size := utf8.DecodeRuneInString(phrase)
	if r == utf8.RuneError || size >= len(phrase) {
		return phrase
	}
	return string(r) + combiningGraphemeJoiner + phrase[size:]
}

// obfuscateText replaces all sensitive words in the text.
func (m *SensitiveWordMatcher) obfuscateText(text string) string {
	if m == nil || m.regex == nil {
		return text
	}
	if m.obfuscate != nil {
		return m.regex.ReplaceAllStringFunc(text, m.obfuscate)
	}
	return m.regex.ReplaceAllStringFunc(text, obfuscateWord)
}

// Matches reports whether the text contains any of the configured sensitive words.
func (m *SensitiveWordMatcher) Matches(text string) bool {
	if m == nil || m.regex == nil {
		return false
	}
	return m.regex.MatchString(text)
}

// ObfuscateText replaces all sensitive words in the text.
func (m *SensitiveWordMatcher) ObfuscateText(text string) string {
	return m.obfuscateText(text)
}

// ObfuscateSensitiveWords processes the payload and obfuscates sensitive words
// in system blocks and message content.
func ObfuscateSensitiveWords(payload []byte, matcher *SensitiveWordMatcher) []byte {
	if matcher == nil || matcher.regex == nil {
		return payload
	}

	// Obfuscate in system blocks
	payload = obfuscateSystemBlocks(payload, matcher)

	// Obfuscate in messages
	payload = obfuscateMessages(payload, matcher)

	return payload
}

// ObfuscateSensitiveWordsInSystemInstruction obfuscates sensitive words in an Antigravity system instruction.
func ObfuscateSensitiveWordsInSystemInstruction(payload []byte, matcher *SensitiveWordMatcher) []byte {
	if matcher == nil || matcher.regex == nil {
		return payload
	}

	for _, path := range []string{"request.systemInstruction", "request.system_instruction"} {
		instruction := gjson.GetBytes(payload, path)
		if !instruction.Exists() {
			continue
		}
		if instruction.Type == gjson.String {
			text := instruction.String()
			if obfuscated := matcher.obfuscateText(text); obfuscated != text {
				payload, _ = sjson.SetBytes(payload, path, obfuscated)
			}
			continue
		}

		parts := instruction.Get("parts")
		if !parts.IsArray() {
			continue
		}
		parts.ForEach(func(key, part gjson.Result) bool {
			if part.Get("text").Type != gjson.String {
				return true
			}
			text := part.Get("text").String()
			if obfuscated := matcher.obfuscateText(text); obfuscated != text {
				payload, _ = sjson.SetBytes(payload, path+".parts."+key.String()+".text", obfuscated)
			}
			return true
		})
	}

	return payload
}

// obfuscateSystemBlocks obfuscates sensitive words in system blocks.
func obfuscateSystemBlocks(payload []byte, matcher *SensitiveWordMatcher) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	if system.IsArray() {
		modified := false
		system.ForEach(func(key, value gjson.Result) bool {
			if value.Get("type").String() == "text" {
				text := value.Get("text").String()
				if strings.HasPrefix(text, "x-anthropic-billing-header:") {
					return true
				}
				obfuscated := matcher.obfuscateText(text)
				if obfuscated != text {
					path := "system." + key.String() + ".text"
					payload, _ = sjson.SetBytes(payload, path, obfuscated)
					modified = true
				}
			}
			return true
		})
		if modified {
			return payload
		}
	} else if system.Type == gjson.String {
		text := system.String()
		if !strings.HasPrefix(text, "x-anthropic-billing-header:") {
			obfuscated := matcher.obfuscateText(text)
			if obfuscated != text {
				payload, _ = sjson.SetBytes(payload, "system", obfuscated)
			}
		}
	}

	return payload
}

// obfuscateMessages obfuscates sensitive words in message content.
func obfuscateMessages(payload []byte, matcher *SensitiveWordMatcher) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	messages.ForEach(func(msgKey, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.Exists() {
			return true
		}

		msgPath := "messages." + msgKey.String()

		if content.Type == gjson.String {
			// Simple string content
			text := content.String()
			obfuscated := matcher.obfuscateText(text)
			if obfuscated != text {
				payload, _ = sjson.SetBytes(payload, msgPath+".content", obfuscated)
			}
		} else if content.IsArray() {
			// Array of content blocks
			content.ForEach(func(blockKey, block gjson.Result) bool {
				if block.Get("type").String() == "text" {
					text := block.Get("text").String()
					obfuscated := matcher.obfuscateText(text)
					if obfuscated != text {
						path := msgPath + ".content." + blockKey.String() + ".text"
						payload, _ = sjson.SetBytes(payload, path, obfuscated)
					}
				}
				return true
			})
		}

		return true
	})

	return payload
}
