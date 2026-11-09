package bcmr

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	// hints, the authbase or the links let go.
	Older *Publication

	// Burned reports that the head ended the authchain.
	Burned bool

	// AuthbaseScripts are what the authbase's output 0 can pay to, rebuilt
	// from the genesis at GenesisHeight, to look for the authbase before it
	// for a publication on it, once, which AuthbaseSearched records.  A token
	// started from the hints has none, as the hints have its publication.
	AuthbaseScripts  [][]byte
	GenesisHeight    int32
	AuthbaseSearched bool

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
	// publication's registry couldn't be read or had nothing to use, or that
	// the metadata is from before, as the registry read again had nothing to
	// use.
	RegistryFailed bool

	// RetryHash is the hash of a registry that couldn't be read, RetryTries
	// how many times in a row, and RetryLast when last, so that the wait
	// before the next try outlasts a restart.
	RetryHash  [32]byte
	RetryTries uint
	RetryLast  time.Time
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
			s.Older = pub
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
// bcmrNFTsBucket the one their NFT types were kept in, which they're moved out
// of: the types can be megabytes, and the database, which holds the keys, would
// keep the room they took for good, so they're in files of their own.
var (
	bcmrBucket     = []byte("bcmr")
	bcmrNFTsBucket = []byte("bcmrnfts")
)

// Store keeps states in the wallet's database, and the NFT types of each in a
// file of its own in typesDir, which are read when asked for, the last few kept.
// The files take no more than typesLimit bytes, if it's above 0: past it, those
// asked for longest ago are evicted.
type Store struct {
	db         walletdb.DB
	typesDir   string
	typesLimit int64

	// typesMu guards the types files and recent, the types read last, newest
	// first, as any goroutine can ask for them.
	typesMu sync.Mutex
	recent  []recentTypes

	// last and lastTypes hold what was last written of each state and of its
	// NFT types, so that what didn't change costs no write.  They belong to
	// the Manager's goroutine.
	last      map[chainhash.Hash][sha256.Size]byte
	lastTypes map[chainhash.Hash][sha256.Size]byte
}

// NewStore returns a store in the database, with the NFT types in typesDir, up
// to typesLimit bytes of them or with no limit if it's 0, adding its bucket to a
// wallet that doesn't have it yet, and moving the types that were kept in the
// database to their files.
func NewStore(db walletdb.DB, typesDir string, typesLimit int64) (*Store, error) {
	if err := os.MkdirAll(typesDir, 0700); err != nil {
		return nil, err
	}
	s := &Store{
		db:         db,
		typesDir:   typesDir,
		typesLimit: typesLimit,
		last:       make(map[chainhash.Hash][sha256.Size]byte),
		lastTypes:  make(map[chainhash.Hash][sha256.Size]byte),
	}
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		_, err := tx.CreateTopLevelBucket(bcmrBucket)
		if err != nil && !errors.Is(err, walletdb.ErrBucketExists) {
			return err
		}
		old := tx.ReadWriteBucket(bcmrNFTsBucket)
		if old == nil {
			return nil
		}
		err = old.ForEach(func(key, value []byte) error {
			if len(key) != chainhash.HashSize {
				return nil
			}
			var category chainhash.Hash
			copy(category[:], key)
			return s.writeTypes(category, value)
		})
		if err != nil {
			return err
		}
		return tx.DeleteTopLevelBucket(bcmrNFTsBucket)
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// recentTypes are the NFT types of a token that were read.
type recentTypes struct {
	category chainhash.Hash
	types    map[string]NFTType
}

// maxRecentTypes is how many tokens' NFT types the store keeps read.
const maxRecentTypes = 4

// NFTTypes returns a token's NFT types, which only its file holds, or nil if
// there are none, or they can't be read, or were evicted: a file that can't be
// read is removed.  The file is marked as asked for now, which keeps it longest.
func (s *Store) NFTTypes(category chainhash.Hash) map[string]NFTType {
	s.typesMu.Lock()
	defer s.typesMu.Unlock()
	now := time.Now()
	_ = os.Chtimes(s.typesFile(category), now, now)
	for i, recent := range s.recent {
		if recent.category == category {
			s.recent = slices.Insert(slices.Delete(s.recent, i, i+1), 0, recent)
			return recent.types
		}
	}
	types, ok := s.readTypes(category)
	if !ok {
		_ = os.Remove(s.typesFile(category))
		return nil
	}
	s.recent = slices.Insert(s.recent, 0, recentTypes{category, types})
	if len(s.recent) > maxRecentTypes {
		s.recent = s.recent[:maxRecentTypes]
	}
	return types
}

// changeTypes writes a token's NFT types, as gob encodes them, or removes its
// file if there are none, and drops what was read of them.  It holds typesMu
// throughout, so that a read never keeps the types from before, nor removes a
// file it couldn't read just as this one replaces it.
func (s *Store) changeTypes(category chainhash.Hash, encoded []byte) error {
	s.typesMu.Lock()
	defer s.typesMu.Unlock()
	s.recent = slices.DeleteFunc(s.recent, func(recent recentTypes) bool {
		return recent.category == category
	})
	if len(encoded) != 0 {
		if err := s.writeTypes(category, encoded); err != nil {
			return err
		}
		s.evictTypes(category)
		return nil
	}
	err := os.Remove(s.typesFile(category))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// evictTypes removes the types files asked for longest ago until they take no
// more room than the limit, but for the one just stored and those of the types
// read last, which show.  An evicted token's types are read from its registry
// again when they're asked for.  typesMu must be held.
func (s *Store) evictTypes(stored chainhash.Hash) {
	if s.typesLimit <= 0 {
		return
	}
	entries, err := os.ReadDir(s.typesDir)
	if err != nil {
		log.Warnf("Unable to read the NFT types: %v", err)
		return
	}
	type typesFile struct {
		category chainhash.Hash
		size     int64
		asked    time.Time
	}
	var files []typesFile
	var total int64
	for _, entry := range entries {
		hex, ok := strings.CutSuffix(entry.Name(), ".gob")
		category, err := chainhash.NewHashFromStr(hex)
		if !ok || err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		files = append(files, typesFile{*category, info.Size(), info.ModTime()})
	}
	slices.SortFunc(files, func(a, b typesFile) int { return a.asked.Compare(b.asked) })
	for _, file := range files {
		if total <= s.typesLimit {
			return
		}
		shown := slices.ContainsFunc(s.recent, func(recent recentTypes) bool {
			return recent.category == file.category
		})
		if file.category == stored || shown {
			continue
		}
		if err := os.Remove(s.typesFile(file.category)); err != nil {
			continue
		}
		total -= file.size
	}
}

// typesFile returns the file of a token's NFT types.
func (s *Store) typesFile(category chainhash.Hash) string {
	return filepath.Join(s.typesDir, category.String()+".gob")
}

// writeTypes writes a token's NFT types, as gob encodes them, in full or not at
// all, as a file that's renamed into place once it's on the disk.
func (s *Store) writeTypes(category chainhash.Hash, encoded []byte) error {
	file, err := os.CreateTemp(s.typesDir, ".types-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(encoded); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), s.typesFile(category))
}

// readTypes returns a token's NFT types, or nil if it has none, and whether
// what's there could be read.
func (s *Store) readTypes(category chainhash.Hash) (map[string]NFTType, bool) {
	encoded, err := os.ReadFile(s.typesFile(category))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	var types map[string]NFTType
	if gob.NewDecoder(bytes.NewReader(encoded)).Decode(&types) != nil {
		return nil, false
	}
	return types, true
}

// typesVersion returns what tells whether a state's NFT types changed, which
// they only do when a registry is read.  It doesn't look at the types, as gob
// writes a map in a random order, and they can be megabytes.
func typesVersion(state *State) [sha256.Size]byte {
	types := 0
	var nextSnapshot int64
	if state.Metadata != nil {
		types = state.Metadata.NFTTypeCount
		nextSnapshot = state.Metadata.NextSnapshot.UnixNano()
	}
	version := fmt.Sprintf("%x %d %d %d %d", state.RegistryHash, state.RegistryRead.UnixNano(),
		state.RegistryParser, types, nextSnapshot)
	return sha256.Sum256([]byte(version))
}

// Put stores a state.
func (s *Store) Put(state *State) error {
	return s.PutAll([]*State{state})
}

// PutAll stores states, writing those that changed to the database in one
// transaction.  One whose NFT types can't be stored isn't, and the others are.
func (s *Store) PutAll(states []*State) error {
	type put struct {
		category        chainhash.Hash
		value           []byte
		digest, version [sha256.Size]byte
	}
	var errs []error
	puts := make([]put, 0, len(states))
	for _, state := range states {
		light := *state
		var types map[string]NFTType
		if state.Metadata != nil {
			metadata := *state.Metadata
			types, metadata.NFTTypes = metadata.NFTTypes, nil
			light.Metadata = &metadata
		}
		var value bytes.Buffer
		if err := gob.NewEncoder(&value).Encode(&light); err != nil {
			errs = append(errs, fmt.Errorf("token %v: %w", state.Category, err))
			continue
		}
		digest := sha256.Sum256(value.Bytes())
		version := typesVersion(state)
		last, stored := s.last[state.Category]
		lastTypes, storedTypes := s.lastTypes[state.Category]
		sameState := stored && last == digest
		sameTypes := storedTypes && lastTypes == version
		if sameState && sameTypes && len(types) == 0 {
			continue
		}
		if err := s.putTypes(state, types, sameTypes); err != nil {
			errs = append(errs, fmt.Errorf("token %v: %w", state.Category, err))
			continue
		}
		// Types are only written apart from their state when it's as stored.
		if !sameState {
			puts = append(puts, put{state.Category, value.Bytes(), digest, version})
		}
	}
	if len(puts) == 0 {
		return errors.Join(errs...)
	}
	err := walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		bucket := tx.ReadWriteBucket(bcmrBucket)
		for _, put := range puts {
			if err := bucket.Put(put.category[:], put.value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, put := range puts {
		s.last[put.category], s.lastTypes[put.category] = put.digest, put.version
	}
	return errors.Join(errs...)
}

// putTypes stores a state's NFT types.  Types are only held once read from a
// registry, until they're stored, and are written even when they're as before,
// as their file may have been evicted.  Types that aren't held are as their file
// has them, which goes when there are none.
func (s *Store) putTypes(state *State, types map[string]NFTType, sameTypes bool) error {
	switch {
	case len(types) != 0:
		var encoded bytes.Buffer
		if err := gob.NewEncoder(&encoded).Encode(types); err != nil {
			return err
		}
		return s.changeTypes(state.Category, encoded.Bytes())

	case !sameTypes && (state.Metadata == nil || state.Metadata.NFTTypeCount == 0):
		return s.changeTypes(state.Category, nil)
	}
	return nil
}

// Delete removes a state and its NFT types.  The types go first, so that one
// that's left has the registry read again.
func (s *Store) Delete(category chainhash.Hash) error {
	if err := s.changeTypes(category, nil); err != nil {
		return err
	}
	err := walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		return tx.ReadWriteBucket(bcmrBucket).Delete(category[:])
	})
	if err != nil {
		return err
	}
	delete(s.last, category)
	delete(s.lastTypes, category)
	return nil
}

// All returns every state.  A state that can't be read is left out, and is
// found again, and NFT types that can't be read are read from the registry
// again.
func (s *Store) All() (map[chainhash.Hash]*State, error) {
	states := make(map[chainhash.Hash]*State)
	err := walletdb.View(s.db, func(tx walletdb.ReadTx) error {
		return tx.ReadBucket(bcmrBucket).ForEach(func(key, value []byte) error {
			var state State
			err := gob.NewDecoder(bytes.NewReader(value)).Decode(&state)
			if err != nil || len(key) != chainhash.HashSize {
				return nil
			}
			copy(state.Category[:], key)
			states[state.Category] = &state

			// The types stay in their file until they're asked for.  A state
			// from before they were kept apart has them with it, and they
			// go to their file now, the state at its first save.  Types
			// whose file is gone, as evicted, are read from the registry
			// again when they're asked for.
			if state.Metadata != nil && state.Metadata.NFTTypes != nil {
				types := state.Metadata.NFTTypes
				state.Metadata.NFTTypes, state.Metadata.NFTTypeCount = nil, len(types)
				var encoded bytes.Buffer
				if gob.NewEncoder(&encoded).Encode(types) != nil ||
					s.writeTypes(state.Category, encoded.Bytes()) != nil {

					state.RegistryParser = 0
				}
				return nil
			}
			// A state from when the types were in a bucket of their own has
			// no count of them, though their file, which NewStore wrote, has
			// them: it's counted from the file, once, and written anew.
			if state.Metadata != nil && state.Metadata.NFTTypeCount == 0 {
				if _, err := os.Stat(s.typesFile(state.Category)); err == nil {
					if types, ok := s.readTypes(state.Category); ok && len(types) != 0 {
						state.Metadata.NFTTypeCount = len(types)
						return nil
					}
				}
			}
			// Any other is as was last written, so a save that changes
			// nothing writes nothing.
			s.last[state.Category] = sha256.Sum256(value)
			s.lastTypes[state.Category] = typesVersion(&state)
			return nil
		})
	})
	return states, err
}
