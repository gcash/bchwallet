package bcmr

import (
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

	// maxNFTTypesSize bounds the size of the NFT types kept from a registry,
	// which is how a registry could otherwise fill a reply to the app.
	maxNFTTypesSize = 1 << 20
)

// symbolPattern is the CHIP's rule for a token's symbol.
var symbolPattern = regexp.MustCompile(`^[-A-Z0-9]+$`)

// ErrNoMetadata is the error of a registry that has no usable metadata for a
// token.
var ErrNoMetadata = errors.New("the registry has no metadata for the token")

// NFTType is what a registry says of an NFT by its commitment.
type NFTType struct {
	Name        string
	Description string
	IconURI     string
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

	// NFTTypes has the sequential NFTs the registry names, by the hex of
	// their commitments.
	NFTTypes map[string]NFTType
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
				} `json:"types"`
			} `json:"parse"`
		} `json:"nfts"`
	} `json:"token"`
}

// ParseRegistry returns what a registry says of a token as of now.  Only the
// identity whose authbase is the category counts: a registry published on a
// token's own authchain speaks for that token, and whatever else it holds has
// no such backing.  It returns ErrNoMetadata if the registry has nothing it can
// use for the token.
func ParseRegistry(data []byte, category chainhash.Hash, now time.Time) (*Metadata, error) {
	var registry registryJSON
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("invalid registry: %w", err)
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
	var registry registryJSON
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("invalid registry: %w", err)
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
	var currentTime, oldestTime time.Time
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
	return metadataOf(&snapshot, category)
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
	if !displayable(name, maxNameRunes) || name == "" {
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
	if nfts := token.NFTs; nfts != nil && nfts.Parse.Bytecode == nil {
		commitments := make([]string, 0, len(nfts.Parse.Types))
		for commitment := range nfts.Parse.Types {
			commitments = append(commitments, commitment)
		}
		sort.Strings(commitments)
		size := 0
		for _, commitment := range commitments {
			nft := nfts.Parse.Types[commitment]
			nftName := firstLine(nft.Name)
			if !displayable(nftName, maxNameRunes) || nftName == "" {
				continue
			}
			if metadata.NFTTypes == nil {
				metadata.NFTTypes = make(map[string]NFTType)
			}
			nftType := NFTType{Name: nftName, IconURI: usableURI(nft.URIs["icon"])}
			if displayable(nft.Description, maxDescriptionRunes) {
				nftType.Description = nft.Description
			}
			size += len(commitment) + len(nftType.Name) + len(nftType.Description) +
				len(nftType.IconURI)
			if size > maxNFTTypesSize {
				break
			}
			metadata.NFTTypes[commitment] = nftType
		}
	}
	return metadata, nil
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
