package bcmr

import "testing"

// TestSkeleton ensures names that read alike have one skeleton, and names that
// don't, others.
func TestSkeleton(t *testing.T) {
	same := [][]string{
		{"Moria USD", "Mоria  USD", "moria-usd", "M0RIA USD", "Móriá USD"},
		{"BCH", "ＢＣＨ", "B.C.H", "ВСН"},
		{"Bitcoin", "Bitcoіn", "B1tcoin", "Bltcoin"},
		{"SOV", "S0V", "5OV"},
		{"Quack", "Ԛuack"},
	}
	for _, names := range same {
		for _, name := range names[1:] {
			if skeleton(name) != skeleton(names[0]) {
				t.Errorf("%q reads as %q, %q as %q", name, skeleton(name), names[0], skeleton(names[0]))
			}
		}
	}
	for _, pair := range [][2]string{{"Moria USD", "Mars USD"}, {"BCH", "BCHN"}, {"東京", "京都"}} {
		if skeleton(pair[0]) == skeleton(pair[1]) {
			t.Errorf("%q reads as %q", pair[0], pair[1])
		}
	}
}

// TestWithinOneEdit ensures strings one character apart, by adding, removing or
// changing one, are within one edit, and those further apart aren't.
func TestWithinOneEdit(t *testing.T) {
	for _, pair := range [][2]string{{"broach", "broach"}, {"broach", "broaches"[:7]},
		{"broach", "brach"}, {"broach", "bruach"}, {"broach", "xbroach"}, {"broach", "broacx"}} {
		if !withinOneEdit(pair[0], pair[1]) || !withinOneEdit(pair[1], pair[0]) {
			t.Errorf("%q and %q aren't within one edit", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{{"broach", "brxxch"}, {"broach", "bro"}, {"broach", "rboach"}} {
		if withinOneEdit(pair[0], pair[1]) {
			t.Errorf("%q and %q are within one edit", pair[0], pair[1])
		}
	}
}

// TestNameCopies ensures a name copies a Known one that it is, or for one long
// enough, that it holds or is one character from, and that a short Known name
// is only copied whole.
func TestNameCopies(t *testing.T) {
	for _, pair := range [][2]string{{"broach", "broach"}, {"broachofficial", "broach"},
		{"broaches", "broache"}, {"broech", "broach"}, {"sov", "sov"}} {
		if !nameCopies(pair[0], pair[1]) {
			t.Errorf("%q doesn't copy %q", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{{"sovs", "sov"}, {"mysov", "sov"}, {"brooch", "broach"[:5]}} {
		if nameCopies(pair[0], pair[1]) {
			t.Errorf("%q copies %q", pair[0], pair[1])
		}
	}
}

// TestReadsAsAmount ensures an NFT's name that reads as an amount of one of the
// symbols does, and others don't.
func TestReadsAsAmount(t *testing.T) {
	symbols := []string{symbolSkeleton("BCH"), symbolSkeleton("MUSD")}
	for _, name := range []string{"10 BCH", "+500 MUSD", "-1,000.5 bch", "\u2212 2 BCH", "3 B-CH", "4 BCH-X", "7 MU5D", "9MUSD"} {
		if !readsAsAmount(name, symbols) {
			t.Errorf("%q doesn't read as an amount", name)
		}
	}
	for _, name := range []string{"Ninja #10", "BCH Ninja 10", "10 Ninjas", "Reaper 4322", "10 BCH Ninja"} {
		if readsAsAmount(name, symbols) {
			t.Errorf("%q reads as an amount", name)
		}
	}
}

// TestSymbolCopiesInOtherForms ensures a symbol that reads as a reserved one in
// another script, in fullwidth forms or with characters that don't show copies
// it, and that one that only looks like none doesn't.
func TestSymbolCopiesInOtherForms(t *testing.T) {
	for _, symbol := range []string{"ВСН", "ＢＣＨ", "B\u200bCH", "B.C.H", "bch", "ＵＳＤＴ", "UЅDТ", "ВІТ", "S0L", "ЅOL"} {
		if !symbolCopies(symbol, reservedSymbolSkeletons) {
			t.Errorf("%q doesn't copy a reserved symbol", symbol)
		}
	}
	for _, symbol := range []string{"FBTC", "HASH", "東京", "ROACH", "BLT", "UNL", "IEO", "ITC", "SOI", "FLL"} {
		if symbolCopies(symbol, reservedSymbolSkeletons) {
			t.Errorf("%q copies a reserved symbol", symbol)
		}
	}
}
