package boot

import (
	"path/filepath"
	"testing"

	"github.com/gcash/bchwallet/netparams"
)

func TestNetworkParams(t *testing.T) {
	tests := []struct {
		cfg    config
		params *netparams.Params
		dir    string
	}{
		{config{}, &netparams.MainNetParams, "mainnet"},
		{config{TestNet3: true}, &netparams.TestNet3Params, "testnet"},
		{config{TestNet4: true}, &netparams.TestNet4Params, "testnet4"},
		{config{ChipNet: true}, &netparams.ChipNetParams, "chipnet"},
		{config{Regtest: true}, &netparams.RegtestParams, "regtest"},
		{config{SimNet: true}, &netparams.SimNetParams, "simnet"},
	}
	for _, test := range tests {
		params, err := networkParams(&test.cfg)
		if err != nil {
			t.Errorf("%s: %v", test.dir, err)
			continue
		}
		if params != test.params {
			t.Errorf("%s: got the %s params", test.dir, params.Name)
		}
		if dir := networkDir("data", params.Params); dir != filepath.Join("data", test.dir) {
			t.Errorf("%s: wallet directory %s", test.dir, dir)
		}
	}

	if _, err := networkParams(&config{TestNet4: true, ChipNet: true}); err == nil {
		t.Error("two networks were accepted")
	}
}
