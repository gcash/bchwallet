package bcmr

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"fmt"
	"time"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchwallet/walletdb"
)

// Status is how far the wallet got with a token's identity.
type Status int

const (
	// StatusSearching is a token whose identity isn't followed to the tip
	// yet.  What it shows is from the newest publication found so far.
	StatusSearching Status = iota

	// StatusVerified is a token whose identity is followed to the tip, by
	// the wallet's filters or by the full node it trusts.
	StatusVerified

	// StatusUnavailable is a token the backend can't look up, as a full node
	// without its BCMR index can't.
	StatusUnavailable

	// StatusFailed is a token whose identity couldn't be found where it had
	// to be.
	StatusFailed
)

// Publication is a link's BCMR publication.
type Publication struct {
	Hash [32]byte
	URIs []string
}

// Link is a transaction of an authchain.
type Link struct {
	Txid      chainhash.Hash
	Height    int32
	BlockHash chainhash.Hash

	// Publication is the link's publication, if it has a well-formed one.
	Publication *Publication
}

// State is what the wallet knows of a token's identity.
type State struct {
	Category chainhash.Hash

	// Links are the links the wallet holds, oldest first, the last being the
	// head.  The first is where following began: the hints' head, or the
	// genesis.
	Links []Link

	// Older is the newest publication from before the first link, from the
	// hints, as its txid and height, until it's read.
	OlderTxid   *chainhash.Hash
	OlderHeight int32
	Older       *Publication

	// Burned reports that the head ended the authchain.
	Burned bool

	// ScannedTo and ScannedHash are the height and hash of the block the
	// head is known unspent through.
	ScannedTo   int32
	ScannedHash chainhash.Hash

	// Status is how far the wallet got.
	Status Status

	// RegistryHash is the hash of the registry Metadata came from.
	RegistryHash [32]byte
	Metadata     *Metadata

	// RegistryChanged reports that Metadata is from a file that isn't the
	// published one, and RegistryRead when that file was read.
	RegistryChanged bool
	RegistryRead    time.Time

	// RegistryParser is the registryParser that read Metadata.
	RegistryParser int

	// RegistryFailed reports that there's no metadata to show because the
	// publication's registry couldn't be read or had nothing to use.
	RegistryFailed bool
}

// Head returns the head, or nil if none is known yet.
func (s *State) Head() *Link {
	if len(s.Links) == 0 {
		return nil
	}
	return &s.Links[len(s.Links)-1]
}

// CurrentPublication returns the publication to read: the newest of the links,
// or else the one from before them.
func (s *State) CurrentPublication() *Publication {
	for i := len(s.Links) - 1; i >= 0; i-- {
		if pub := s.Links[i].Publication; pub != nil {
			return pub
		}
	}
	return s.Older
}

// clearRegistry forgets what was read of a registry.
func (s *State) clearRegistry() {
	s.RegistryHash, s.Metadata = [32]byte{}, nil
	s.RegistryChanged, s.RegistryFailed = false, false
	s.RegistryRead = time.Time{}
}

// retryInterval is how long something that couldn't be read waits.
const retryInterval = 10 * time.Minute

// maxLinks is how many links a state keeps.  The links are there for reorgs, and
// a chain through a busy contract can have any number of them.
const maxLinks = 256

// trimLinks drops the oldest links past maxLinks, keeping a publication among
// them as the older one.
func (s *State) trimLinks() {
	excess := len(s.Links) - maxLinks
	if excess <= 0 {
		return
	}
	for i := excess - 1; i >= 0; i-- {
		if pub := s.Links[i].Publication; pub != nil {
			txid := s.Links[i].Txid
			s.Older, s.OlderTxid, s.OlderHeight = pub, &txid, s.Links[i].Height
			break
		}
	}
	s.Links = append([]Link(nil), s.Links[excess:]...)
}

// PublicationStale returns whether the publication to read isn't on the head,
// so the head may mean a newer registry that wasn't published.
func (s *State) PublicationStale() bool {
	head := s.Head()
	return head != nil && head.Publication == nil && s.CurrentPublication() != nil
}

// bcmrBucket is the wallet database's bucket for token identities, and
// bcmrNFTsBucket the one for the NFT types of their metadata.  The types can be
// megabytes, and a state changes with every block, so they're kept apart.
var (
	bcmrBucket     = []byte("bcmr")
	bcmrNFTsBucket = []byte("bcmrnfts")
)

// Store keeps states in the wallet's database.
type Store struct {
	db walletdb.DB

	// last and lastTypes hold what was last written of each state and of its
	// NFT types, so that what didn't change costs no write.  They belong to
	// the Manager's goroutine.
	last      map[chainhash.Hash][sha256.Size]byte
	lastTypes map[chainhash.Hash][sha256.Size]byte
}

// NewStore returns a store in the database, adding its buckets to a wallet that
// doesn't have them yet.
func NewStore(db walletdb.DB) (*Store, error) {
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		for _, bucket := range [][]byte{bcmrBucket, bcmrNFTsBucket} {
			_, err := tx.CreateTopLevelBucket(bucket)
			if err != nil && !errors.Is(err, walletdb.ErrBucketExists) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Store{
		db:        db,
		last:      make(map[chainhash.Hash][sha256.Size]byte),
		lastTypes: make(map[chainhash.Hash][sha256.Size]byte),
	}, nil
}

// typesVersion returns what tells whether a state's NFT types changed, which
// they only do when a registry is read.  It doesn't look at the types, as gob
// writes a map in a random order, and they can be megabytes.
func typesVersion(state *State) [sha256.Size]byte {
	types := 0
	var nextSnapshot int64
	if state.Metadata != nil {
		types = len(state.Metadata.NFTTypes)
		nextSnapshot = state.Metadata.NextSnapshot.UnixNano()
	}
	version := fmt.Sprintf("%x %d %d %d %d", state.RegistryHash, state.RegistryRead.UnixNano(),
		state.RegistryParser, types, nextSnapshot)
	return sha256.Sum256([]byte(version))
}

// Put stores a state.
func (s *Store) Put(state *State) error {
	light := *state
	var types map[string]NFTType
	if state.Metadata != nil {
		metadata := *state.Metadata
		types, metadata.NFTTypes = metadata.NFTTypes, nil
		light.Metadata = &metadata
	}
	var value bytes.Buffer
	if err := gob.NewEncoder(&value).Encode(&light); err != nil {
		return err
	}
	digest := sha256.Sum256(value.Bytes())
	version := typesVersion(state)
	last, stored := s.last[state.Category]
	lastTypes, storedTypes := s.lastTypes[state.Category]
	sameState := stored && last == digest
	sameTypes := storedTypes && lastTypes == version
	if sameState && sameTypes {
		return nil
	}
	var typesValue bytes.Buffer
	if !sameTypes && len(types) != 0 {
		if err := gob.NewEncoder(&typesValue).Encode(types); err != nil {
			return err
		}
	}
	err := walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		if !sameState {
			err := tx.ReadWriteBucket(bcmrBucket).Put(state.Category[:], value.Bytes())
			if err != nil {
				return err
			}
		}
		if sameTypes {
			return nil
		}
		bucket := tx.ReadWriteBucket(bcmrNFTsBucket)
		if len(types) == 0 {
			return bucket.Delete(state.Category[:])
		}
		return bucket.Put(state.Category[:], typesValue.Bytes())
	})
	if err == nil {
		s.last[state.Category], s.lastTypes[state.Category] = digest, version
	}
	return err
}

// All returns every state.  A state that can't be read is left out, and is
// found again, and NFT types that can't be read are read from the registry
// again.
func (s *Store) All() (map[chainhash.Hash]*State, error) {
	states := make(map[chainhash.Hash]*State)
	err := walletdb.View(s.db, func(tx walletdb.ReadTx) error {
		nfts := tx.ReadBucket(bcmrNFTsBucket)
		return tx.ReadBucket(bcmrBucket).ForEach(func(key, value []byte) error {
			var state State
			err := gob.NewDecoder(bytes.NewReader(value)).Decode(&state)
			if err != nil || len(key) != chainhash.HashSize {
				return nil
			}
			copy(state.Category[:], key)
			states[state.Category] = &state

			// A state from before the types were kept apart has them with
			// it, and is written anew at its first save.
			stored := nfts.Get(key)
			if state.Metadata == nil || stored == nil {
				return nil
			}
			var types map[string]NFTType
			if gob.NewDecoder(bytes.NewReader(stored)).Decode(&types) != nil {
				state.RegistryParser = 0
				return nil
			}
			state.Metadata.NFTTypes = types
			s.last[state.Category] = sha256.Sum256(value)
			s.lastTypes[state.Category] = typesVersion(&state)
			return nil
		})
	})
	return states, err
}
