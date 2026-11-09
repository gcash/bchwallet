package bcmr

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// lookAlikeLetters are letters of other scripts that read as Latin ones, and
// characters that read as one another, each with the one form both take in a
// skeleton.  It holds the ones that names are made of; the full list of
// confusables is far larger.
var lookAlikeLetters = map[rune]rune{
	// Cyrillic.
	'а': 'a', 'А': 'a', 'в': 'b', 'В': 'b', 'е': 'e', 'Е': 'e', 'з': '3', 'З': '3',
	'і': 'l', 'І': 'l', 'ј': 'j', 'Ј': 'j', 'к': 'k', 'К': 'k', 'м': 'm', 'М': 'm',
	'н': 'h', 'Н': 'h', 'о': 'o', 'О': 'o', 'р': 'p', 'Р': 'p', 'с': 'c', 'С': 'c',
	'т': 't', 'Т': 't', 'у': 'y', 'У': 'y', 'х': 'x', 'Х': 'x', 'ѕ': 's', 'Ѕ': 's',
	'ԁ': 'd', 'һ': 'h', 'ӏ': 'l', 'Ӏ': 'l', 'ԛ': 'q', 'ԝ': 'w', 'ү': 'y', 'Ү': 'y',

	// Greek.
	'α': 'a', 'Α': 'a', 'β': 'b', 'Β': 'b', 'ε': 'e', 'Ε': 'e', 'ζ': 'z', 'Ζ': 'z',
	'η': 'n', 'Η': 'h', 'ι': 'l', 'Ι': 'l', 'κ': 'k', 'Κ': 'k', 'μ': 'm', 'Μ': 'm',
	'ν': 'v', 'Ν': 'n', 'ο': 'o', 'Ο': 'o', 'ρ': 'p', 'Ρ': 'p', 'τ': 't', 'Τ': 't',
	'υ': 'u', 'Υ': 'y', 'χ': 'x', 'Χ': 'x',

	// Digits and letters.
	'0': 'o', '1': 'l', 'i': 'l', 'I': 'l', '|': 'l', '5': 's', '8': 'b',
}

// skeleton returns what a name reads as, so that names that read alike have one
// skeleton: compatibility forms and accents gone, letters that read as others
// made those, case folded, and spaces and punctuation left out.  Fullwidth
// "ＢＣＨ", "Bitcoin  Cash" and "BitcоinCash" with a Cyrillic "о" read alike.
func skeleton(name string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(name) {
		switch {
		case unicode.In(r, unicode.Mn, unicode.Me):
			continue
		case unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.Is(unicode.Cf, r):
			continue
		}
		if like, ok := lookAlikeLetters[r]; ok {
			r = like
		} else {
			r = unicode.ToLower(r)
			if like, ok := lookAlikeLetters[r]; ok {
				r = like
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// reservedNames are names that no token can rightly have, as they're the coin's
// own, by their skeletons.
var reservedNames = func() map[string]bool {
	names := make(map[string]bool)
	for _, name := range []string{"Bitcoin Cash", "Bitcoin", "BCH"} {
		names[skeleton(name)] = true
	}
	return names
}()

// minNearKnownName is the shortest skeleton of a Known name that a name one
// character from it, or holding it, copies.  Shorter ones would match far too
// many names that copy nothing.
const minNearKnownName = 6

// nameCopies returns whether a name's skeleton copies a Known name's: the same,
// one character from it, or holding it, for a Known name long enough.
func nameCopies(name, known string) bool {
	if name == known {
		return true
	}
	if len([]rune(known)) < minNearKnownName {
		return false
	}
	return strings.Contains(name, known) || withinOneEdit(name, known)
}

// symbolSkeleton returns what a symbol reads as: the digits that read as letters
// made those, and hyphens left out.  The CHIP has symbols in uppercase ASCII.
func symbolSkeleton(symbol string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(symbol) {
		switch r {
		case '-':
			continue
		case '0':
			r = 'O'
		case '1':
			r = 'I'
		case '5':
			r = 'S'
		case '8':
			r = 'B'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// minNearSymbol is the shortest symbol that a symbol one character from it
// copies.  Shorter ones would match far too many that copy nothing.
const minNearSymbol = 4

// symbolCopies returns whether a symbol, whole or up to its first hyphen,
// reads as one of the symbols' skeletons, or is one character from one of
// minNearSymbol or more characters.
func symbolCopies(symbol string, symbols []string) bool {
	forms := []string{symbolSkeleton(symbol), symbolSkeleton(baseSymbol(symbol))}
	for _, other := range symbols {
		for _, form := range forms {
			if form == other || len(other) >= minNearSymbol && withinOneEdit(form, other) {
				return true
			}
		}
	}
	return false
}

// reservedSymbolSkeletons are the reserved symbols' skeletons.
var reservedSymbolSkeletons = func() []string {
	skeletons := make([]string, len(reservedSymbolList))
	for i, symbol := range reservedSymbolList {
		skeletons[i] = symbolSkeleton(symbol)
	}
	return skeletons
}()

// withinOneEdit returns whether a can be made b by adding, removing or changing
// one character.
func withinOneEdit(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	if len(ra) < len(rb) {
		ra, rb = rb, ra
	}
	if len(ra)-len(rb) > 1 {
		return false
	}
	i := 0
	for i < len(rb) && ra[i] == rb[i] {
		i++
	}
	if i == len(rb) {
		return true
	}
	if len(ra) == len(rb) {
		return string(ra[i+1:]) == string(rb[i+1:])
	}
	return string(ra[i+1:]) == string(rb[i:])
}
