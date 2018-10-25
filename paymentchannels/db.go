package paymentchannels

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/walletdb"
)

var (
	paymentChannelBucket = []byte("paymentchannels")
	openChannelsBucket   = []byte("openchannels")
	closedChannelsBucket = []byte("closedchannels")
	transactionsBucket   = []byte("transactions")
)

func init() {
	gob.Register(bchec.KoblitzCurve{})
	gob.Register(bchutil.AddressScriptHash{})
}

// initDatabase will attempt to create all of the database bucks if they do not
// yet exist.
func initDatabase(db walletdb.DB) error {
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		wb, err := tx.CreateTopLevelBucket(paymentChannelBucket)
		if err != nil {
			return err
		}
		if _, err = wb.CreateBucketIfNotExists(openChannelsBucket); err != nil {
			return err
		}
		if _, err = wb.CreateBucketIfNotExists(closedChannelsBucket); err != nil {
			return err
		}
		if _, err = wb.CreateBucketIfNotExists(transactionsBucket); err != nil {
			return err
		}
		return nil
	})
	if err != nil && err != walletdb.ErrBucketExists {
		return err
	}
	return nil
}

func serializeChannel(c Channel) ([]byte, error) {
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	encoder := gob.NewEncoder(w)

	if err := encoder.Encode(c); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func deserializeChannel(ser []byte) (*Channel, error) {
	r := bytes.NewReader(ser)
	decoder := gob.NewDecoder(r)

	var c Channel
	if err := decoder.Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}
