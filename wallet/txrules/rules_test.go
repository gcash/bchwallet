package txrules

import (
	"bytes"
	"testing"

	"github.com/gcash/bchd/wire"
)

// TestTokenOutputDust checks that the dust threshold of an output counts its
// tokens, as bchd's does, and stays the same for an output without tokens.
func TestTokenOutputDust(t *testing.T) {
	script := append(append([]byte{0x76, 0xa9, 0x14}, bytes.Repeat([]byte{0x42}, 20)...),
		0x88, 0xac)
	plain := wire.NewTxOut(546, script, wire.TokenData{})
	token := wire.NewTxOut(546, script, wire.TokenData{
		CategoryID: [32]byte{1},
		Amount:     1000,
		BitField:   wire.HAS_AMOUNT,
	})

	if IsDustOutput(plain, DefaultRelayFeePerKb) ||
		!IsDustOutput(wire.NewTxOut(545, script, wire.TokenData{}),
			DefaultRelayFeePerKb) {

		t.Error("the dust limit of an output without tokens moved from 546 " +
			"satoshis")
	}
	if !IsDustOutput(token, DefaultRelayFeePerKb) {
		t.Error("an output of 546 satoshis with tokens isn't dust")
	}

	// At its threshold, the output passes bchd's rule: value * 1000 /
	// (3 * (size + 148)) not below the relay fee.
	threshold := OutputDustThreshold(token, DefaultRelayFeePerKb)
	token.Value = int64(threshold)
	size := int64(token.SerializeSize() + 148)
	if IsDustOutput(token, DefaultRelayFeePerKb) ||
		token.Value*1000/(3*size) < int64(DefaultRelayFeePerKb) {

		t.Errorf("an output with tokens of %d satoshis, its threshold, "+
			"is dust", token.Value)
	}
	token.Value--
	if !IsDustOutput(token, DefaultRelayFeePerKb) {
		t.Errorf("an output with tokens of %d satoshis isn't dust",
			token.Value)
	}
}
