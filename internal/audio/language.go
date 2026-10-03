package audio

import "strings"

// CanonicalLanguage spells a BCP-47 tag in its canonical case: language
// lower, script title, region upper ("EN_us" → "en-US", "cmn-hans-cn" →
// "cmn-Hans-CN"). Providers disagree on case — Google answers "en-us" — and
// a client should see one spelling whichever tier served it. An empty tag
// stays empty.
func CanonicalLanguage(tag string) string {
	tag = strings.TrimSpace(strings.ReplaceAll(tag, "_", "-"))
	if tag == "" {
		return ""
	}
	parts := strings.Split(tag, "-")
	for i, p := range parts {
		switch {
		case i == 0:
			parts[i] = strings.ToLower(p)
		case len(p) == 4 && isLetters(p):
			parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
		case len(p) == 2 && isLetters(p), len(p) == 3 && !isLetters(p):
			parts[i] = strings.ToUpper(p)
		default:
			parts[i] = strings.ToLower(p)
		}
	}
	return strings.Join(parts, "-")
}

func isLetters(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// PrimaryLanguage is a tag's language subtag alone, lower case ("en-US" →
// "en"): the form the Whisper family accepts and answers in.
func PrimaryLanguage(tag string) string {
	primary, _, _ := strings.Cut(CanonicalLanguage(tag), "-")
	return primary
}

// ForLanguage looks a language up in a map keyed by BCP-47 tags: the exact
// tag first, then its primary language, so a key "en" covers every English
// region and a key "en-US" wins over it for that region. Keys match in any
// case.
func ForLanguage(m map[string]string, tag string) (string, bool) {
	if len(m) == 0 || tag == "" {
		return "", false
	}
	want, primary := CanonicalLanguage(tag), PrimaryLanguage(tag)
	var fallback string
	var found bool
	for k, v := range m {
		switch CanonicalLanguage(k) {
		case want:
			return v, true
		case primary:
			fallback, found = v, true
		}
	}
	return fallback, found
}

// LanguageFromName turns the language a Whisper-family provider reports —
// a lower-case English name ("russian"), which is what OpenAI, Groq and
// whisper.cpp put in verbose output — into its language code. A value that
// is already a code is returned in canonical case; a name this table does
// not know is returned empty rather than passed on as if it were a tag.
func LanguageFromName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ""
	}
	if code, ok := whisperLanguageNames[n]; ok {
		return code
	}
	if len(n) <= 3 || strings.Contains(n, "-") {
		return CanonicalLanguage(n)
	}
	return ""
}

// whisperLanguageNames is the Whisper tokenizer's language table, name →
// code, plus the alternative names it accepts. Javanese is the one entry
// where Whisper's own code ("jw") is not the ISO one; the ISO code is used.
var whisperLanguageNames = map[string]string{
	"afrikaans": "af", "albanian": "sq", "amharic": "am", "arabic": "ar",
	"armenian": "hy", "assamese": "as", "azerbaijani": "az", "bashkir": "ba",
	"basque": "eu", "belarusian": "be", "bengali": "bn", "bosnian": "bs",
	"breton": "br", "bulgarian": "bg", "burmese": "my", "cantonese": "yue",
	"castilian": "es", "catalan": "ca", "chinese": "zh", "croatian": "hr",
	"czech": "cs", "danish": "da", "dutch": "nl", "english": "en",
	"estonian": "et", "faroese": "fo", "finnish": "fi", "flemish": "nl",
	"french": "fr", "galician": "gl", "georgian": "ka", "german": "de",
	"greek": "el", "gujarati": "gu", "haitian": "ht", "haitian creole": "ht",
	"hausa": "ha", "hawaiian": "haw", "hebrew": "he", "hindi": "hi",
	"hungarian": "hu", "icelandic": "is", "indonesian": "id", "italian": "it",
	"japanese": "ja", "javanese": "jv", "kannada": "kn", "kazakh": "kk",
	"khmer": "km", "korean": "ko", "lao": "lo", "latin": "la",
	"latvian": "lv", "letzeburgesch": "lb", "lingala": "ln", "lithuanian": "lt",
	"luxembourgish": "lb", "macedonian": "mk", "malagasy": "mg", "malay": "ms",
	"malayalam": "ml", "maltese": "mt", "mandarin": "zh", "maori": "mi",
	"marathi": "mr", "moldavian": "ro", "moldovan": "ro", "mongolian": "mn",
	"myanmar": "my", "nepali": "ne", "norwegian": "no", "nynorsk": "nn",
	"occitan": "oc", "panjabi": "pa", "pashto": "ps", "persian": "fa",
	"polish": "pl", "portuguese": "pt", "punjabi": "pa", "pushto": "ps",
	"romanian": "ro", "russian": "ru", "sanskrit": "sa", "serbian": "sr",
	"shona": "sn", "sindhi": "sd", "sinhala": "si", "sinhalese": "si",
	"slovak": "sk", "slovenian": "sl", "somali": "so", "spanish": "es",
	"sundanese": "su", "swahili": "sw", "swedish": "sv", "tagalog": "tl",
	"tajik": "tg", "tamil": "ta", "tatar": "tt", "telugu": "te",
	"thai": "th", "tibetan": "bo", "turkish": "tr", "turkmen": "tk",
	"ukrainian": "uk", "urdu": "ur", "uzbek": "uz", "valencian": "ca",
	"vietnamese": "vi", "welsh": "cy", "yiddish": "yi", "yoruba": "yo",
}
