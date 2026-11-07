package bcmr

import (
	"crypto/sha256"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
)

// authbaseScripts returns the output scripts that output 0 of a token's authbase
// can have, from the genesis input that spends it, for a search of the filters
// before the genesis: P2PKH from the public key the input gives, and P2SH, by its
// 20 and 32 byte hashes, from the redeem script.  It returns none for an input it
// can't read, such as one that isn't only pushes, which no other kind of output
// is spent with.
func authbaseScripts(genesis *wire.MsgTx, category chainhash.Hash) [][]byte {
	var sigScript []byte
	found := false
	for _, in := range genesis.TxIn {
		if in.PreviousOutPoint == (wire.OutPoint{Hash: category}) {
			sigScript, found = in.SignatureScript, true
			break
		}
	}
	if !found {
		return nil
	}
	pushes, err := txscript.PushedData(sigScript)
	if err != nil || len(pushes) == 0 || !txscript.IsPushOnlyScript(sigScript) {
		return nil
	}
	last := pushes[len(pushes)-1]
	if len(last) == 0 {
		return nil
	}

	var scripts [][]byte
	if len(pushes) == 2 && isPublicKey(last) {
		scripts = append(scripts, p2pkh(bchutil.Hash160(last)))
	}
	scripts = append(scripts, p2sh(bchutil.Hash160(last)))
	redeemHash := sha256.Sum256(last)
	redeemHash = sha256.Sum256(redeemHash[:])
	return append(scripts, p2sh32(redeemHash[:]))
}

// isPublicKey returns whether a push is shaped as a public key: compressed, or
// not.
func isPublicKey(push []byte) bool {
	switch len(push) {
	case 33:
		return push[0] == 0x02 || push[0] == 0x03
	case 65:
		return push[0] == 0x04
	}
	return false
}

func p2pkh(hash []byte) []byte {
	script := []byte{txscript.OP_DUP, txscript.OP_HASH160, txscript.OP_DATA_20}
	script = append(script, hash...)
	return append(script, txscript.OP_EQUALVERIFY, txscript.OP_CHECKSIG)
}

func p2sh(hash []byte) []byte {
	script := []byte{txscript.OP_HASH160, txscript.OP_DATA_20}
	script = append(script, hash...)
	return append(script, txscript.OP_EQUAL)
}

func p2sh32(hash []byte) []byte {
	script := []byte{txscript.OP_HASH256, txscript.OP_DATA_32}
	script = append(script, hash...)
	return append(script, txscript.OP_EQUAL)
}
