// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collation

// acceptedLocales is the set of collation locales MongoDB 8.0 accepts (109 ICU
// locale IDs). DumboDB validates against this so it rejects the same locales
// MongoDB does; every one is tailored under the vendored ICU (verified in
// go-icu-collation's fingerprint test).
var acceptedLocales = map[string]struct{}{}

func init() {
	for _, loc := range []string{
		"af", "sq", "am", "ar", "hy", "as", "az", "bn", "be", "bs", "bs_Cyrl",
		"bg", "my", "ca", "chr", "zh", "zh_Hant", "hr", "cs", "da", "nl", "dz",
		"en", "en_US", "en_US_POSIX", "eo", "et", "ee", "fo", "fil", "fi", "fr",
		"fr_CA", "gl", "ka", "de", "de_AT", "el", "gu", "ha", "haw", "he", "hi",
		"hu", "is", "ig", "smn", "id", "ga", "it", "ja", "kl", "kn", "kk", "km",
		"kok", "ko", "ky", "lkt", "lo", "lv", "ln", "lt", "dsb", "lb", "mk", "ms",
		"ml", "mt", "mr", "mn", "ne", "se", "nb", "nn", "or", "om", "ps", "fa",
		"fa_AF", "pl", "pt", "pa", "ro", "ru", "sr", "sr_Latn", "si", "sk", "sl",
		"es", "sw", "sv", "ta", "te", "th", "bo", "to", "tr", "uk", "hsb", "ur",
		"ug", "vi", "wae", "cy", "yi", "yo", "zu",
	} {
		acceptedLocales[loc] = struct{}{}
	}
}

// LocaleAccepted reports whether locale is one DumboDB accepts for collation.
// The simple/binary collation ("" or "simple") is always accepted; otherwise the
// locale must be in MongoDB's accepted set.
func LocaleAccepted(locale string) bool {
	if locale == "" || locale == "simple" {
		return true
	}
	_, ok := acceptedLocales[locale]
	return ok
}
