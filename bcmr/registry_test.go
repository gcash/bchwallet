package bcmr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg/chainhash"
)

var testCategory = chainhash.Hash{1, 2, 3}

// testRegistry returns a registry with the snapshot's JSON at a past time,
// with the token's category filled in for %s.
func testRegistry(snapshot string) []byte {
	return []byte(fmt.Sprintf(`{"identities":{"%s":{"2024-01-01T00:00:00.000Z":%s}}}`,
		testCategory, fmt.Sprintf(snapshot, testCategory)))
}

var testNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestParseRegistry ensures the fields of a token are read.
func TestParseRegistry(t *testing.T) {
	m, err := ParseRegistry(testRegistry(`{"name":"Test Token\nmore","description":"About it",
		"uris":{"icon":"ipfs://bafkrei1","web":"https://example.com","other":"https://x"},
		"token":{"category":"%s","symbol":"TEST-1","decimals":8,"nfts":{"parse":{"types":{
			"01":{"name":"One","description":"First","uris":{"icon":"https://example.com/1.png"}},
			"02":{"name":"Bad\u202eName"}}}}}}`), testCategory, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "Test Token" || m.Description != "About it" || m.Symbol != "TEST-1" ||
		m.Decimals != 8 || m.IconURI != "ipfs://bafkrei1" || m.WebURI != "https://example.com" {

		t.Fatalf("got %+v", m)
	}
	if len(m.NFTTypes) != 1 || m.NFTTypes["01"].Name != "One" ||
		m.NFTTypes["01"].IconURI != "https://example.com/1.png" {

		t.Fatalf("NFT types %+v", m.NFTTypes)
	}
}

// TestParseRegistryDefaults ensures decimals default to 0 and the optional
// fields can be left out.
func TestParseRegistryDefaults(t *testing.T) {
	m, err := ParseRegistry(testRegistry(`{"name":"T","token":{"category":"%s","symbol":"T"}}`),
		testCategory, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.Decimals != 0 || m.IconURI != "" || m.NFTTypes != nil {
		t.Fatalf("got %+v", m)
	}
}

// TestParseRegistryRefuses ensures a snapshot that would mislead isn't used.
func TestParseRegistryRefuses(t *testing.T) {
	for name, snapshot := range map[string]string{
		"no token":          `{"name":"T%.0s"}`,
		"no name":           `{"token":{"category":"%s","symbol":"T"}}`,
		"blank name":        `{"name":"  ","token":{"category":"%s","symbol":"T"}}`,
		"lowercase symbol":  `{"name":"T","token":{"category":"%s","symbol":"t"}}`,
		"space in symbol":   `{"name":"T","token":{"category":"%s","symbol":"A B"}}`,
		"empty symbol":      `{"name":"T","token":{"category":"%s","symbol":""}}`,
		"decimals too high": `{"name":"T","token":{"category":"%s","symbol":"T","decimals":19}}`,
		"negative decimals": `{"name":"T","token":{"category":"%s","symbol":"T","decimals":-1}}`,
		"reversed name":     `{"name":"T\u202eX","token":{"category":"%s","symbol":"T"}}`,
		"control in name":   `{"name":"T\u0007","token":{"category":"%s","symbol":"T"}}`,
		"long name": `{"name":"` + strings.Repeat("a", maxNameRunes+1) +
			`","token":{"category":"%s","symbol":"T"}}`,
		"another category": `{"name":"T%.0s","token":{"category":"00","symbol":"T"}}`,
	} {
		_, err := ParseRegistry(testRegistry(snapshot), testCategory, testNow)
		if !errors.Is(err, ErrNoMetadata) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := ParseRegistry([]byte("{not json"), testCategory, testNow); err == nil {
		t.Error("invalid JSON was read")
	}
	decimalFloat := testRegistry(`{"name":"T","token":{"category":"%s","symbol":"T","decimals":1.5}}`)
	if _, err := ParseRegistry(decimalFloat, testCategory, testNow); err == nil {
		t.Error("fractional decimals were read")
	}
}

// TestParseRegistryDropsOptional ensures an unusable optional value is left
// out without losing the token.
func TestParseRegistryDropsOptional(t *testing.T) {
	m, err := ParseRegistry(testRegistry(`{"name":"T","description":"a\u202eb",
		"uris":{"icon":"http://example.com/i.png","web":"javascript:alert(1)"},
		"token":{"category":"%s","symbol":"T"}}`), testCategory, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.Description != "" || m.IconURI != "" || m.WebURI != "" {
		t.Fatalf("got %+v", m)
	}
}

// TestParseRegistryOtherIdentities ensures only the token's own identity
// counts: a registry on one token's authchain can't speak for another.
func TestParseRegistryOtherIdentities(t *testing.T) {
	other := chainhash.Hash{9}
	data := []byte(fmt.Sprintf(`{"identities":{"%s":{"2024-01-01T00:00:00.000Z":
		{"name":"Fake","token":{"category":"%s","symbol":"FAKE"}}}}}`, other, testCategory))
	if _, err := ParseRegistry(data, testCategory, testNow); !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("got %v", err)
	}
}

// TestParseRegistrySnapshots ensures the latest snapshot reached is current,
// that one in the future waits, that the oldest counts if none is reached, and
// that a key that isn't a time is ignored.
func TestParseRegistrySnapshots(t *testing.T) {
	snapshot := func(name string) string {
		return fmt.Sprintf(`{"name":"%s","token":{"category":"%s","symbol":"T"}}`, name, testCategory)
	}
	data := []byte(fmt.Sprintf(`{"identities":{"%s":{
		"2020-01-01T00:00:00.000Z":%s,
		"2025-06-01T00:00:00.000Z":%s,
		"2024-01-01T00:00:00.000Z":%s,
		"2030-01-01T00:00:00.000Z":%s,
		"later":%s}}}`, testCategory, snapshot("Old"), snapshot("Current"), snapshot("Mid"),
		snapshot("Future"), snapshot("Junk")))

	for _, test := range []struct {
		now  time.Time
		want string
	}{
		{testNow, "Current"},
		{time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC), "Future"},
		{time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), "Old"},
		{time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), "Current"},
	} {
		m, err := ParseRegistry(data, testCategory, test.now)
		if err != nil || m.Name != test.want {
			t.Fatalf("at %v: got %+v, %v", test.now, m, err)
		}
	}
}

// TestParseRegistrySkipsParsableNFTs ensures NFTs a bytecode parses aren't read
// as if their types were commitments.
func TestParseRegistrySkipsParsableNFTs(t *testing.T) {
	m, err := ParseRegistry(testRegistry(`{"name":"T","token":{"category":"%s","symbol":"T",
		"nfts":{"parse":{"bytecode":"00","types":{"01":{"name":"One"}}}}}}`), testCategory, testNow)
	if err != nil || m.NFTTypes != nil {
		t.Fatalf("got %+v, %v", m, err)
	}
}

// TestParseRegistryEmptyBytecode ensures a collection with an empty bytecode is
// sequential, as Gambling Apes Club and Punk Apes Club have it.
func TestParseRegistryEmptyBytecode(t *testing.T) {
	data := testRegistry(`{"name":"T","token":{"category":"%s","symbol":"T","nfts":{"parse":
		{"bytecode":"","types":{"0001":{"name":"One"}}}}}}`)
	m, err := ParseRegistry(data, testCategory, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.NFTTypes["0001"].Name != "One" {
		t.Fatalf("%+v", m.NFTTypes)
	}
}

// TestParseRegistryAttributes ensures an NFT's attributes are kept, and that one
// that can't show only loses itself.
func TestParseRegistryAttributes(t *testing.T) {
	data := testRegistry(`{"name":"T","token":{"category":"%s","symbol":"T","nfts":{"parse":{"types":{
		"01":{"name":"One","extensions":{"attributes":{"Eyes":"Red","Level":7,"None":null,"Bad":"a\u202eb"}}},
		"02":{"name":"Two","extensions":{"attributes":"not a map"}},
		"03":{"name":"Three","extensions":[]}}}}}}`)
	m, err := ParseRegistry(data, testCategory, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.NFTTypes["01"].Attributes; len(got) != 1 || got["Eyes"] != "Red" {
		t.Fatalf("%+v", got)
	}
	if two := m.NFTTypes["02"]; two.Name != "Two" || two.Attributes != nil {
		t.Fatalf("%+v", two)
	}
	if three := m.NFTTypes["03"]; three.Name != "Three" || three.Attributes != nil {
		t.Fatalf("%+v", three)
	}
}

// TestParseKnownList ensures each usable token of a list is read under its
// category, and the rest are left out.
func TestParseKnownList(t *testing.T) {
	other := chainhash.Hash{7}
	data := []byte(fmt.Sprintf(`{"identities":{
		"%s":{"2024-01-01T00:00:00.000Z":{"name":"One","token":{"category":"%s","symbol":"ONE","decimals":2}}},
		"%s":{"2024-01-01T00:00:00.000Z":{"name":"Bad","token":{"category":"%s","symbol":"bad"}}},
		"nonsense":{"2024-01-01T00:00:00.000Z":{"name":"X"}}}}`,
		testCategory, testCategory, other, other))
	known, err := ParseKnownList(data, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(known) != 1 || known[testCategory].Symbol != "ONE" || known[testCategory].Decimals != 2 {
		t.Fatalf("got %+v", known)
	}
}

// TestParseRegistryNFTTypesBounded ensures a registry can't fill a reply to the
// app with NFT types.
func TestParseRegistryNFTTypesBounded(t *testing.T) {
	var types strings.Builder
	long := strings.Repeat("d", maxDescriptionRunes)
	for i := range 10000 {
		if i > 0 {
			types.WriteString(",")
		}
		fmt.Fprintf(&types, `"%04x":{"name":"N","description":"%s"}`, i, long)
	}
	data := testRegistry(`{"name":"T","token":{"category":"%s","symbol":"T","nfts":{"parse":{"types":{` +
		types.String() + `}}}}}`)
	m, err := ParseRegistry(data, testCategory, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.NFTTypes) == 0 || len(m.NFTTypes) >= 10000 {
		t.Fatalf("%d NFT types", len(m.NFTTypes))
	}
	if _, ok := m.NFTTypes["0000"]; !ok {
		t.Fatal("the first commitments weren't the ones kept")
	}
}

// TestParseRegistryRefusesTooManyEntries ensures a registry can't take all of a
// phone's memory with short keys.
func TestParseRegistryRefusesTooManyEntries(t *testing.T) {
	data := []byte(strings.Repeat(":", maxRegistryEntries+1))
	if _, err := ParseRegistry(data, testCategory, testNow); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("got %v", err)
	}
}

// TestParseRegistryNextSnapshot ensures a snapshot still to come is noted.
func TestParseRegistryNextSnapshot(t *testing.T) {
	data := []byte(fmt.Sprintf(`{"identities":{"%s":{
		"2024-01-01T00:00:00.000Z":{"name":"Now","token":{"category":"%s","symbol":"T"}},
		"2030-01-01T00:00:00.000Z":{"name":"Later","token":{"category":"%s","symbol":"T"}}}}}`,
		testCategory, testCategory, testCategory))
	m, err := ParseRegistry(data, testCategory, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "Now" || m.NextSnapshot.Year() != 2030 {
		t.Fatalf("%+v", m)
	}
}

// TestSymbolPattern ensures a symbol follows the CHIP's rule.
func TestSymbolPattern(t *testing.T) {
	for symbol, want := range map[string]bool{"MUSD": true, "MUSD-A": true, "-BCH": false, "---": false, "a": false} {
		if symbolPattern.MatchString(symbol) != want {
			t.Errorf("%q", symbol)
		}
	}
}

// TestParseRegistryNFTImage ensures an NFT's image is kept beside its icon.
func TestParseRegistryNFTImage(t *testing.T) {
	data := testRegistry(`{"name":"T","token":{"category":"%s","symbol":"T","nfts":{"parse":{"types":{
		"01":{"name":"One","uris":{"icon":"ipfs://icon","image":"ipfs://image"}}}}}}}`)
	m, err := ParseRegistry(data, testCategory, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.NFTTypes["01"]; got.IconURI != "ipfs://icon" || got.ImageURI != "ipfs://image" {
		t.Fatalf("%+v", got)
	}
}

// TestNameable ensures a name with a line or paragraph separator, nothing to
// see, or a letter with more than two combining marks isn't one, and that names
// in any script are.
func TestNameable(t *testing.T) {
	for _, name := range []string{"Moria USD", "Дракон", "東京", "Café", "e\u0301\u0302", "a\u0301\u0302e\u0301\u0302"} {
		if !nameable(name) {
			t.Errorf("%q isn't a name", name)
		}
	}
	for _, name := range []string{"", "   ", "\u3164\u3164", "\u2800", "\uffa0 \u115f\u1160",
		"Moria\u2028USD", "Moria\u2029", "Moria\u0085", "Moria\r", "e\u0301\u0302\u0303"} {
		if nameable(name) {
			t.Errorf("%q is a name", name)
		}
	}
}

// TestParseRegistryRefusesBlankNames ensures a token with a name that shows
// nothing has no metadata, and that an NFT's or an attribute's is left out.
func TestParseRegistryRefusesBlankNames(t *testing.T) {
	category := chainhash.Hash{1}
	blankToken := []byte(fmt.Sprintf(`{"identities":{"%[1]s":{"2024-01-01T00:00:00.000Z":
		{"name":"\u3164","token":{"category":"%[1]s","symbol":"TOK"}}}}}`, category))
	if _, err := ParseRegistry(blankToken, category, time.Now()); err == nil {
		t.Fatal("a blank name was taken")
	}
	collection := []byte(fmt.Sprintf(`{"identities":{"%[1]s":{"2024-01-01T00:00:00.000Z":
		{"name":"Token","token":{"category":"%[1]s","symbol":"TOK","nfts":{"parse":{"bytecode":"",
		"types":{"01":{"name":"\u2800"},"02":{"name":"Two","extensions":{"attributes":
		{"Eyes":"Blue\u2028Fake","Hat":"Red"}}}}}}}}}}}`, category))
	metadata, err := ParseRegistry(collection, category, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata.NFTTypes["01"]; ok {
		t.Fatal("a blank NFT name was taken")
	}
	attributes := metadata.NFTTypes["02"].Attributes
	if _, ok := attributes["Eyes"]; ok || attributes["Hat"] != "Red" {
		t.Fatalf("attributes %v", attributes)
	}
}
