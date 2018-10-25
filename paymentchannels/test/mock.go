package test

import (
	"crypto/rand"
	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/waddrmgr"
	"github.com/gcash/bchwallet/wallet/txauthor"
)

type MockWalletBackend struct {
	params           *chaincfg.Params
	unspentOutpoints map[wire.OutPoint]struct{}
	addrs            map[bchutil.Address]bchec.PrivateKey
}

func NewMockWalletBackend(params *chaincfg.Params) *MockWalletBackend {
	unspent := make(map[wire.OutPoint]struct{})
	addrs := make(map[bchutil.Address]bchec.PrivateKey)
	return &MockWalletBackend{params, unspent, addrs}
}

func (w *MockWalletBackend) NewAddress(account uint32, scope waddrmgr.KeyScope) (bchutil.Address, error) {
	priv, err := bchec.NewPrivateKey(bchec.S256())
	if err != nil {
		return nil, err
	}
	pub := priv.PubKey()
	addr, err := bchutil.NewAddressPubKeyHash(bchutil.Hash160(pub.SerializeCompressed()), w.params)
	if err != nil {
		return nil, err
	}
	w.addrs[addr] = *priv
	return addr, nil
}

func (w *MockWalletBackend) CreateSimpleTx(account uint32, outputs []*wire.TxOut, minconf int32, satPerKb bchutil.Amount) (*txauthor.AuthoredTx, error) {
	tx := wire.NewMsgTx(1)
	b := make([]byte, 32)
	rand.Read(b)
	ch, _ := chainhash.NewHash(b)
	op := wire.NewOutPoint(ch, 0)
	w.unspentOutpoints[*op] = struct{}{}
	tx.TxIn = append(tx.TxIn, wire.NewTxIn(op, nil))
	tx.TxOut = outputs
	authored := &txauthor.AuthoredTx{Tx: tx}
	return authored, nil
}

func (w *MockWalletBackend) PublishTransaction(tx *wire.MsgTx) error {
	return nil
}

func (w *MockWalletBackend) LockOutpoint(op wire.OutPoint) {}

func (w *MockWalletBackend) UnlockOutpoint(op wire.OutPoint) {}
