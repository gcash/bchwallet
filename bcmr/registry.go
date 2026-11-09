package bcmr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gcash/bchd/chaincfg/chainhash"
)

const (
	// maxNameRunes and maxDescriptionRunes bound what a registry can have the
	// app show.  The CHIP has interfaces hide what's past 20 and 140, so these
	// leave plenty of room.
	maxNameRunes        = 200
	maxDescriptionRunes = 2000

	// maxDecimals is the most decimals a token can have, by the CHIP.
	maxDecimals = 18

	// maxURILen bounds a URI of a registry.
	maxURILen = 2048

	// maxRegistryEntries bounds the keys a registry can hold.  A short key
	// costs far more in memory than it does in the file, so a small registry
	// could otherwise take all of a phone's memory.  The largest in use, of
	// 10,000 NFTs with their attributes, has about 200,000.
	maxRegistryEntries = 1 << 20

	// maxNFTTypesSize bounds the size of the NFT types kept from a registry.
	// It's the size of the largest registry, so that no collection loses
	// names, and a reply to the app has to allow for it.
	maxNFTTypesSize = maxRegistrySize
)

// symbolPattern is the CHIP's rule for a token's symbol.
var symbolPattern = regexp.MustCompile(`^[A-Z0-9]+[-A-Z0-9]*$`)

// ErrNoMetadata is the error of a registry that has no usable metadata for a
// token.
var ErrNoMetadata = errors.New("the registry has no metadata for the token")

// NFTType is what a registry says of an NFT by its commitment.
type NFTType struct {
	Name        string
	Description string
	IconURI     string

	// ImageURI is the NFT's art in full, if the registry gives it.
	ImageURI string

	// Attributes are the NFT's traits, by name.
	Attributes map[string]string
}

// Metadata is what a registry says of a token, in the snapshot that is current.
type Metadata struct {
	Name        string
	Description string
	Symbol      string
	Decimals    uint8

	// IconURI and WebURI are the https or ipfs URIs of the token's icon and
	// site, if it has them.
	IconURI string
	WebURI  string

	// NextSnapshot is when a snapshot of the registry that's still to come
	// takes over, if one is.
	NextSnapshot time.Time

	// NFTTypes has the sequential NFTs the registry names, by the hex of
	// their commitments, from when the registry is read until it's stored,
	// after which the store has them, and NFTTypeCount says how many there
	// are.
	NFTTypes     map[string]NFTType
	NFTTypeCount int
}

type registryJSON struct {
	Identities map[string]map[string]json.RawMessage `json:"identities"`
}

type snapshotJSON struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	URIs        map[string]string `json:"uris"`
	Token       *struct {
		Category string `json:"category"`
		Symbol   string `json:"symbol"`
		Decimals *int   `json:"decimals"`
		NFTs     *struct {
			Parse struct {
				Bytecode *string `json:"bytecode"`
				Types    map[string]struct {
					Name        string            `json:"name"`
					Description string            `json:"description"`
					URIs        map[string]string `json:"uris"`
					Extensions  json.RawMessage   `json:"extensions"`
				} `json:"types"`
			} `json:"parse"`
		} `json:"nfts"`
	} `json:"token"`
}

// readRegistry reads a registry that isn't too much to read.
func readRegistry(data []byte) (*registryJSON, error) {
	if bytes.Count(data, []byte(":")) > maxRegistryEntries {
		return nil, errors.New("invalid registry: too many entries")
	}
	var registry registryJSON
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("invalid registry: %w", err)
	}
	return &registry, nil
}

// ParseRegistry returns what a registry says of a token as of now.  Only the
// identity whose authbase is the category counts: a registry published on a
// token's own authchain speaks for that token, and whatever else it holds has
// no such backing.  It returns ErrNoMetadata if the registry has nothing it can
// use for the token.
func ParseRegistry(data []byte, category chainhash.Hash, now time.Time) (*Metadata, error) {
	registry, err := readRegistry(data)
	if err != nil {
		return nil, err
	}
	history, ok := registry.Identities[category.String()]
	if !ok {
		return nil, ErrNoMetadata
	}
	return snapshotMetadata(history, category, now)
}

// ParseKnownList returns the tokens of a registry that lists tokens the way the
// embedded Known list does, each under its category, as of now.  A token it
// can't use is left out.
func ParseKnownList(data []byte, now time.Time) (map[chainhash.Hash]*Metadata, error) {
	registry, err := readRegistry(data)
	if err != nil {
		return nil, err
	}
	known := make(map[chainhash.Hash]*Metadata, len(registry.Identities))
	for authbase, history := range registry.Identities {
		category, err := chainhash.NewHashFromStr(authbase)
		if err != nil {
			continue
		}
		metadata, err := snapshotMetadata(history, *category, now)
		if err != nil {
			continue
		}
		known[*category] = metadata
	}
	return known, nil
}

// snapshotMetadata returns the metadata of the current snapshot of an identity:
// the one of the latest time reached, or the oldest if none is, by the CHIP.
func snapshotMetadata(history map[string]json.RawMessage, category chainhash.Hash,
	now time.Time) (*Metadata, error) {

	var current json.RawMessage
	var currentTime, oldestTime, nextTime time.Time
	var oldest json.RawMessage
	for stamp, raw := range history {
		t, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		if oldest == nil || t.Before(oldestTime) {
			oldest, oldestTime = raw, t
		}
		if !t.After(now) && (current == nil || t.After(currentTime)) {
			current, currentTime = raw, t
		}
		if t.After(now) && (nextTime.IsZero() || t.Before(nextTime)) {
			nextTime = t
		}
	}
	if current == nil {
		current = oldest
	}
	if current == nil {
		return nil, ErrNoMetadata
	}

	var snapshot snapshotJSON
	if err := json.Unmarshal(current, &snapshot); err != nil {
		return nil, fmt.Errorf("invalid snapshot: %w", err)
	}
	metadata, err := metadataOf(&snapshot, category)
	if err != nil {
		return nil, err
	}
	metadata.NextSnapshot = nextTime
	return metadata, nil
}

// metadataOf checks a snapshot.  A value that would mislead, as a symbol or
// decimals could, loses the whole snapshot.  One that's only unusable, as a
// description or an icon can be, is left out.
func metadataOf(snapshot *snapshotJSON, category chainhash.Hash) (*Metadata, error) {
	token := snapshot.Token
	if token == nil || token.Category != category.String() {
		return nil, ErrNoMetadata
	}
	name := firstLine(snapshot.Name)
	if !displayable(name, maxNameRunes) || !nameable(name) {
		return nil, ErrNoMetadata
	}
	if !symbolPattern.MatchString(token.Symbol) || len(token.Symbol) > maxNameRunes {
		return nil, ErrNoMetadata
	}
	decimals := 0
	if token.Decimals != nil {
		decimals = *token.Decimals
	}
	if decimals < 0 || decimals > maxDecimals {
		return nil, ErrNoMetadata
	}

	metadata := &Metadata{
		Name:     name,
		Symbol:   token.Symbol,
		Decimals: uint8(decimals),
		IconURI:  usableURI(snapshot.URIs["icon"]),
		WebURI:   usableURI(snapshot.URIs["web"]),
	}
	if displayable(snapshot.Description, maxDescriptionRunes) {
		metadata.Description = snapshot.Description
	}
	// An empty bytecode parses nothing, so it's as sequential as none.
	if nfts := token.NFTs; nfts != nil && (nfts.Parse.Bytecode == nil || *nfts.Parse.Bytecode == "") {
		commitments := make([]string, 0, len(nfts.Parse.Types))
		for commitment := range nfts.Parse.Types {
			commitments = append(commitments, commitment)
		}
		sort.Strings(commitments)
		size := 0
		for _, commitment := range commitments {
			nft := nfts.Parse.Types[commitment]
			nftName := firstLine(nft.Name)
			if !displayable(nftName, maxNameRunes) || !nameable(nftName) {
				continue
			}
			if metadata.NFTTypes == nil {
				metadata.NFTTypes = make(map[string]NFTType)
			}
			nftType := NFTType{Name: nftName, IconURI: usableURI(nft.URIs["icon"]),
				ImageURI: usableURI(nft.URIs["image"])}
			if displayable(nft.Description, maxDescriptionRunes) {
				nftType.Description = nft.Description
			}
			nftType.Attributes = attributesOf(nft.Extensions)
			size += len(commitment) + len(nftType.Name) + len(nftType.Description) +
				len(nftType.IconURI) + len(nftType.ImageURI)
			for name, value := range nftType.Attributes {
				size += len(name) + len(value)
			}
			if size > maxNFTTypesSize {
				break
			}
			metadata.NFTTypes[commitment] = nftType
		}
	}
	metadata.NFTTypeCount = len(metadata.NFTTypes)
	return metadata, nil
}

// attributesOf returns the attributes in an NFT's extensions that can show.  The
// CHIP has them as strings, and a registry that gives something else only loses
// what it gave.
func attributesOf(extensions json.RawMessage) map[string]string {
	var fields struct {
		Attributes map[string]json.RawMessage `json:"attributes"`
	}
	if json.Unmarshal(extensions, &fields) != nil {
		return nil
	}
	values := fields.Attributes
	var attributes map[string]string
	for name, rawValue := range values {
		var value string
		if json.Unmarshal(rawValue, &value) != nil ||
			!displayable(name, maxNameRunes) || !displayable(value, maxNameRunes) ||
			!nameable(name) || !nameable(value) {

			continue
		}
		if attributes == nil {
			attributes = make(map[string]string)
		}
		attributes[name] = value
	}
	return attributes
}

// firstLine returns text up to its first newline, as the CHIP has interfaces
// hide what's past it.
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimSpace(line)
}

// displayable returns whether text is valid, within the limit, and free of
// characters that change how text around them reads.  Control and format
// characters, such as the ones that reverse text, could make one name read as
// another.  A newline is fine, as it ends what's first shown.
func displayable(text string, maxRunes int) bool {
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxRunes {
		return false
	}
	for _, r := range text {
		if r == '\n' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// maxMarks is the most combining marks a letter of a name can carry, as stacks
// of them run over the text around.
const maxMarks = 2

// nameable returns whether text reads as a name, an NFT's name or an attribute,
// which show on a line of their own: with no line or paragraph separator,
// something to see, and no letter with more than maxMarks combining marks.
func nameable(text string) bool {
	visible := false
	marks := 0
	for _, r := range text {
		switch {
		case r == '\n' || r == '\r' || r == '\u0085' || r == '\u2028' || r == '\u2029':
			return false
		case unicode.In(r, unicode.Mn, unicode.Me):
			if marks++; marks > maxMarks {
				return false
			}
			continue
		}
		marks = 0
		if !blank(r) {
			visible = true
		}
	}
	return visible
}

// blank returns whether a character shows nothing: a space, or a letter that
// fills a place without a mark, as the Hangul fillers and the blank Braille
// pattern do.
func blank(r rune) bool {
	switch r {
	case '\u115f', '\u1160', '\u3164', '\uffa0', '\u2800':
		return true
	}
	return unicode.IsSpace(r)
}

// usableURI returns uri if it's one a client can read, by the CHIP's https and
// ipfs, or else nothing.
func usableURI(uri string) string {
	if len(uri) > maxURILen || !displayable(uri, maxURILen) {
		return ""
	}
	if strings.HasPrefix(uri, "https://") || strings.HasPrefix(uri, "ipfs://") {
		return uri
	}
	return ""
}
