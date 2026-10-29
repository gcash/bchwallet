package chain

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gcash/bchd/btcjson"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
)

// TestRPCClientBlockTxs ensures the RPC client hands over the relevant
// transactions that bchd notifies for a block together, once a notification
// about anything else comes, so that the wallet can record a parent that the
// block lists after its child first.
func TestRPCClientBlockTxs(t *testing.T) {
	c := &RPCClient{
		enqueueNotification: make(chan interface{}, 10),
		quit:                make(chan struct{}),
	}

	newTx := func(prevOut wire.OutPoint) *wire.MsgTx {
		tx := wire.NewMsgTx(1)
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: prevOut})
		tx.AddTxOut(&wire.TxOut{Value: 1000})
		return tx
	}
	parent := newTx(wire.OutPoint{Hash: chainhash.Hash{1}})
	child := newTx(wire.OutPoint{Hash: parent.TxHash()})
	next := newTx(wire.OutPoint{Hash: chainhash.Hash{2}})
	unmined := newTx(wire.OutPoint{Hash: chainhash.Hash{3}})
	last := newTx(wire.OutPoint{Hash: chainhash.Hash{4}})
	names := map[chainhash.Hash]string{
		parent.TxHash():  "parent",
		child.TxHash():   "child",
		next.TxHash():    "next",
		unmined.TxHash(): "unmined",
		last.TxHash():    "last",
	}
	block := func(height int32) *btcjson.BlockDetails {
		return &btcjson.BlockDetails{
			Height: height,
			Hash:   chainhash.Hash{byte(height)}.String(),
			Time:   time.Now().Unix(),
		}
	}

	// bchd can notify a block's transactions in the block's order, which
	// can list a child first, and notifies a transaction that both spends
	// and pays a watched output twice.
	c.onRedeemingTx(bchutil.NewTx(child), block(100))
	c.onRecvTx(bchutil.NewTx(parent), block(100))
	c.onRedeemingTx(bchutil.NewTx(parent), block(100))
	c.onRecvTx(bchutil.NewTx(next), block(101))
	c.onRecvTx(bchutil.NewTx(unmined), nil)
	c.onRecvTx(bchutil.NewTx(last), block(102))
	c.onBlockConnected(&chainhash.Hash{102}, 102, time.Now())

	var got []string
	for len(c.enqueueNotification) > 0 {
		switch n := (<-c.enqueueNotification).(type) {
		case FilteredBlockConnected:
			txs := make([]string, 0, len(n.RelevantTxs))
			for _, rec := range n.RelevantTxs {
				txs = append(txs, names[rec.Hash])
			}
			got = append(got, fmt.Sprintf("block %d: %s", n.Block.Height,
				strings.Join(txs, " ")))
		case RelevantTx:
			if n.Block != nil {
				got = append(got, fmt.Sprintf("tx %s in block %d",
					names[n.TxRecord.Hash], n.Block.Height))
				continue
			}
			got = append(got, "unmined tx "+names[n.TxRecord.Hash])
		case BlockConnected:
			got = append(got, fmt.Sprintf("block %d connected", n.Height))
		default:
			t.Fatalf("unexpected notification %T", n)
		}
	}

	want := []string{
		"block 100: child parent",
		"block 101: next",
		"unmined tx unmined",
		"block 102: last",
		"block 102 connected",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got notifications\n%s\nwant\n%s", strings.Join(got, "\n"),
			strings.Join(want, "\n"))
	}
}
