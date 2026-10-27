package boot

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// TestLoadConfigNoExit ensures that a config that asks the run to stop, or
// that the run can't start with, makes it return instead of exiting the
// process, which is an app's when the wallet is embedded.  A run that exited
// would end the test binary.
func TestLoadConfigNoExit(t *testing.T) {
	args, prompts := Args, ConsolePrompts
	defer func() { Args, ConsolePrompts = args, prompts }()

	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, nil, 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		options      string
		logDir       string
		args         []string
		prompts      bool
		nothingToRun bool
	}{
		{name: "version", args: []string{"--version"}, prompts: true,
			nothingToRun: true},
		{name: "subsystems", options: "debuglevel=show\n", prompts: true,
			nothingToRun: true},
		{name: "log directory", logDir: filepath.Join(notADir, "logs"),
			prompts: true},
		{name: "simulation wallet on regtest", options: "createtemp=1\n",
			prompts: true},
		{name: "create without a console", options: "create=1\n"},
	}
	for i, test := range tests {
		appData := filepath.Join(dir, fmt.Sprint(i))
		logDir := test.logDir
		if logDir == "" {
			logDir = filepath.Join(appData, "logs")
		}
		conf := fmt.Sprintf("[Application Options]\n\nappdata=%s\n"+
			"logdir=%s\nregtest=1\n%s", appData, logDir, test.options)
		path := filepath.Join(dir, fmt.Sprintf("%d.conf", i))
		if err := os.WriteFile(path, []byte(conf), 0600); err != nil {
			t.Fatal(err)
		}
		Args = test.args
		if Args == nil {
			Args = []string{}
		}
		ConsolePrompts = test.prompts

		_, _, err := loadConfig(&path)
		closeLogRotator()
		switch {
		case test.nothingToRun && !errors.Is(err, errNothingToRun):
			t.Errorf("%s: got %v, want nothing to run", test.name, err)
		case !test.nothingToRun && (err == nil || errors.Is(err, errNothingToRun)):
			t.Errorf("%s: got %v, want an error", test.name, err)
		}
	}

	// With nothing to run, the run ends without an error, as the daemon
	// does after showing its version.
	Args = []string{"--version"}
	path := filepath.Join(dir, "0.conf")
	if err := WalletMainContext(context.Background(), &path); err != nil {
		t.Errorf("a run with nothing to run ended with %v", err)
	}
}
