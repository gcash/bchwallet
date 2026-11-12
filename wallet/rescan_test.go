package wallet

import (
	"bytes"
	"testing"

	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/wtxmgr"
)

// TestWatchedOutPointsKeepScripts checks that a rescan watches each unspent
// output by the script stored on the credit, including a bare multisig output,
// and by the first address that script yields.
func TestWatchedOutPointsKeepScripts(t *testing.T) {
	w := newRegtestWallet(t)

	p2pkh := walletScript(t, w)
	var pubs []*bchutil.AddressPubKey
	for range 2 {
		key, err := bchec.NewPrivateKey(bchec.S256())
		if err != nil {
			t.Fatal(err)
		}
		pub, err := bchutil.NewAddressPubKey(
			key.PubKey().SerializeCompressed(), w.chainParams,
		)
		if err != nil {
			t.Fatal(err)
		}
		pubs = append(pubs, pub)
	}
	multisig, err := txscript.MultiSigScript(pubs, 1)
	if err != nil {
		t.Fatal(err)
	}

	credits := []wtxmgr.Credit{
		{
			OutPoint: wire.OutPoint{Hash: chainhash.Hash{1}},
			PkScript: p2pkh,
		},
		{
			OutPoint: wire.OutPoint{Hash: chainhash.Hash{2}, Index: 1},
			PkScript: multisig,
		},
	}
	got, err := w.watchedOutPoints(credits)
	if err != nil {
		t.Fatalf("watchedOutPoints: %v", err)
	}
	if len(got) != len(credits) {
		t.Fatalf("watched %d outpoints, want %d", len(got), len(credits))
	}
	for _, credit := range credits {
		entry, ok := got[credit.OutPoint]
		if !ok {
			t.Fatalf("outpoint %v isn't watched", credit.OutPoint)
		}
		if !bytes.Equal(entry.PkScript, credit.PkScript) {
			t.Fatalf("outpoint %v script is %x, want the credit's script %x",
				credit.OutPoint, entry.PkScript, credit.PkScript)
		}
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			credit.PkScript, w.chainParams,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(addrs) == 0 {
			t.Fatalf("outpoint %v script has no address", credit.OutPoint)
		}
		if entry.Addr.EncodeAddress() != addrs[0].EncodeAddress() {
			t.Fatalf("outpoint %v address is %v, want %v",
				credit.OutPoint, entry.Addr, addrs[0])
		}
	}
}
