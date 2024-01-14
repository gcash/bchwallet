// Copyright (c) 2015-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

// Package rpcserver implements the RPC API and is used by the main package to
// start gRPC services.
//
// Full documentation of the API implemented by this package is maintained in a
// language-agnostic document:
//
//	https://github.com/dcrlabs/bchwallet/blob/master/rpc/documentation/api.md
//
// Any API changes must be performed according to the steps listed here:
//
//	https://github.com/dcrlabs/bchwallet/blob/master/rpc/documentation/serverchanges.md
package rpcserver

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dcrlabs/bchwallet/pymtproto"
	"github.com/dcrlabs/bchwallet/wallet/txsizes"
	"github.com/tyler-smith/go-bip39"
	"google.golang.org/grpc/status"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/dcrlabs/bchwallet/chain"
	"github.com/dcrlabs/bchwallet/internal/cfgutil"
	"github.com/dcrlabs/bchwallet/internal/zero"
	"github.com/dcrlabs/bchwallet/netparams"
	pb "github.com/dcrlabs/bchwallet/rpc/walletrpc"
	"github.com/dcrlabs/bchwallet/waddrmgr"
	"github.com/dcrlabs/bchwallet/wallet"
	"github.com/dcrlabs/bchwallet/walletdb"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/rpcclient"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchutil/hdkeychain"
)

// Public API version constants
const (
	semverString = "2.0.1"
	semverMajor  = 2
	semverMinor  = 0
	semverPatch  = 1
)

// translateError creates a new gRPC error with an appropiate error code for
// recognized errors.
//
// This function is by no means complete and should be expanded based on other
// known errors.  Any RPC handler not returning a gRPC error (with grpc.Errorf)
// should return this result instead.
func translateError(err error) error {
	code := errorCode(err)
	return grpc.Errorf(code, "%s", err.Error())
}

func errorCode(err error) codes.Code {
	// waddrmgr.IsError is convenient, but not granular enough when the
	// underlying error has to be checked.  Unwrap the underlying error
	// if it exists.
	if e, ok := err.(waddrmgr.ManagerError); ok {
		// For these waddrmgr error codes, the underlying error isn't
		// needed to determine the grpc error code.
		switch e.ErrorCode {
		case waddrmgr.ErrWrongPassphrase: // public and private
			return codes.InvalidArgument
		case waddrmgr.ErrAccountNotFound:
			return codes.NotFound
		case waddrmgr.ErrInvalidAccount: // reserved account
			return codes.InvalidArgument
		case waddrmgr.ErrDuplicateAccount:
			return codes.AlreadyExists
		}

		err = e.Err
	}

	switch err {
	case wallet.ErrLoaded:
		return codes.FailedPrecondition
	case walletdb.ErrDbNotOpen:
		return codes.Aborted
	case walletdb.ErrDbExists:
		return codes.AlreadyExists
	case walletdb.ErrDbDoesNotExist:
		return codes.NotFound
	case hdkeychain.ErrInvalidSeedLen:
		return codes.InvalidArgument
	default:
		return codes.Unknown
	}
}

// versionServer provides RPC clients with the ability to query the RPC server
// version.
type versionServer struct {
}

// walletServer provides wallet services for RPC clients.
type walletServer struct {
	ready  uint32 // atomic
	wallet *wallet.Wallet
}

// loaderServer provides RPC clients with the ability to load and close wallets,
// as well as establishing a RPC connection to a bchd consensus server.
type loaderServer struct {
	ready     uint32 // atomic
	loader    *wallet.Loader
	activeNet *netparams.Params
	rpcClient *chain.RPCClient
	mu        sync.Mutex
}

// Singleton implementations of each service.  Not all services are immediately
// usable.
var (
	versionService versionServer
	walletService  walletServer
	loaderService  loaderServer
)

// RegisterServices registers implementations of each gRPC service and registers
// it with the server.  Not all service are ready to be used after registration.
func RegisterServices(server *grpc.Server) {
	pb.RegisterVersionServiceServer(server, &versionService)
	pb.RegisterWalletServiceServer(server, &walletService)
	pb.RegisterWalletLoaderServiceServer(server, &loaderService)
}

var serviceMap = map[string]interface{}{
	"walletrpc.VersionService":      &versionService,
	"walletrpc.WalletService":       &walletService,
	"walletrpc.WalletLoaderService": &loaderService,
}

// ServiceReady returns nil when the service is ready and a gRPC error when not.
func ServiceReady(service string) error {
	s, ok := serviceMap[service]
	if !ok {
		return status.Errorf(codes.Unimplemented, "service %s not found", service)
	}
	type readyChecker interface {
		checkReady() bool
	}
	ready := true
	r, ok := s.(readyChecker)
	if ok {
		ready = r.checkReady()
	}
	if !ready {
		return status.Errorf(codes.FailedPrecondition, "service %v is not ready", service)
	}
	return nil
}

func (*versionServer) Version(ctx context.Context, req *pb.VersionRequest) (*pb.VersionResponse, error) {
	return &pb.VersionResponse{
		VersionString: semverString,
		Major:         semverMajor,
		Minor:         semverMinor,
		Patch:         semverPatch,
	}, nil
}

// StartWalletService creates an implementation of the WalletService and
// registers it with the gRPC server.
func StartWalletService(server *grpc.Server, wallet *wallet.Wallet) {
	walletService.wallet = wallet
	if atomic.SwapUint32(&walletService.ready, 1) != 0 {
		panic("service already started")
	}
}

func (s *walletServer) checkReady() bool {
	return atomic.LoadUint32(&s.ready) != 0
}

func (s *walletServer) Ping(ctx context.Context, req *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{}, nil
}

func (s *walletServer) Network(ctx context.Context, req *pb.NetworkRequest) (
	*pb.NetworkResponse, error) {

	if s.wallet.ChainClient() == nil {
		return nil, translateError(errors.New("chain client to available yet"))
	}

	bestHash, bestHeight, err := s.wallet.ChainClient().GetBestBlock()
	if err != nil {
		return nil, err
	}

	return &pb.NetworkResponse{
		ActiveNetwork: uint32(s.wallet.ChainParams().Net),
		BestBlock:     bestHash.String(),
		BestHeight:    bestHeight,
		SyncedTo:      s.wallet.Manager.SyncedTo().Height,
	}, nil
}

func (s *walletServer) AccountNumber(ctx context.Context, req *pb.AccountNumberRequest) (
	*pb.AccountNumberResponse, error) {

	accountNum, err := s.wallet.AccountNumber(waddrmgr.KeyScopeBIP0044, req.AccountName)
	if err != nil {
		return nil, translateError(err)
	}

	return &pb.AccountNumberResponse{AccountNumber: accountNum}, nil
}

func (s *walletServer) Accounts(ctx context.Context, req *pb.AccountsRequest) (
	*pb.AccountsResponse, error) {

	resp, err := s.wallet.Accounts(waddrmgr.KeyScopeBIP0044)
	if err != nil {
		return nil, translateError(err)
	}
	accounts := make([]*pb.AccountsResponse_Account, len(resp.Accounts))
	for i := range resp.Accounts {
		a := &resp.Accounts[i]
		accounts[i] = &pb.AccountsResponse_Account{
			AccountNumber:    a.AccountNumber,
			AccountName:      a.AccountName,
			TotalBalance:     int64(a.TotalBalance),
			ExternalKeyCount: a.ExternalKeyCount,
			InternalKeyCount: a.InternalKeyCount,
			ImportedKeyCount: a.ImportedKeyCount,
		}
	}
	return &pb.AccountsResponse{
		Accounts:           accounts,
		CurrentBlockHash:   resp.CurrentBlockHash[:],
		CurrentBlockHeight: resp.CurrentBlockHeight,
	}, nil
}

func (s *walletServer) RenameAccount(ctx context.Context, req *pb.RenameAccountRequest) (
	*pb.RenameAccountResponse, error) {

	err := s.wallet.RenameAccount(waddrmgr.KeyScopeBIP0044, req.AccountNumber, req.NewName)
	if err != nil {
		return nil, translateError(err)
	}

	return &pb.RenameAccountResponse{}, nil
}

func (s *walletServer) NextAccount(ctx context.Context, req *pb.NextAccountRequest) (
	*pb.NextAccountResponse, error) {

	defer zero.Bytes(req.Passphrase)

	if req.AccountName == "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "account name may not be empty")
	}

	lock := make(chan time.Time, 1)
	defer func() {
		lock <- time.Time{} // send matters, not the value
	}()
	err := s.wallet.Unlock(req.Passphrase, lock)
	if err != nil {
		return nil, translateError(err)
	}

	account, err := s.wallet.NextAccount(waddrmgr.KeyScopeBIP0044, req.AccountName)
	if err != nil {
		return nil, translateError(err)
	}

	return &pb.NextAccountResponse{AccountNumber: account}, nil
}

func (s *walletServer) NextAddress(ctx context.Context, req *pb.NextAddressRequest) (
	*pb.NextAddressResponse, error) {

	var (
		addr bchutil.Address
		err  error
	)
	switch req.Kind {
	case pb.NextAddressRequest_BIP0044_EXTERNAL:
		addr, err = s.wallet.NewAddress(req.Account, waddrmgr.KeyScopeBIP0044)
	case pb.NextAddressRequest_BIP0044_INTERNAL:
		addr, err = s.wallet.NewChangeAddress(req.Account, waddrmgr.KeyScopeBIP0044)
	default:
		return nil, grpc.Errorf(codes.InvalidArgument, "kind=%v", req.Kind)
	}
	if err != nil {
		return nil, translateError(err)
	}

	return &pb.NextAddressResponse{Address: addr.EncodeAddress()}, nil
}

func (s *walletServer) CurrentAddress(ctx context.Context, req *pb.CurrentAddressRequest) (
	*pb.CurrentAddressResponse, error) {

	addr, err := s.wallet.CurrentAddress(req.Account, waddrmgr.KeyScopeBIP0044)
	if err != nil {
		return nil, translateError(err)
	}

	return &pb.CurrentAddressResponse{Address: addr.EncodeAddress()}, nil
}

func (s *walletServer) ImportPrivateKey(ctx context.Context, req *pb.ImportPrivateKeyRequest) (
	*pb.ImportPrivateKeyResponse, error) {

	defer zero.Bytes(req.Passphrase)

	wif, err := bchutil.DecodeWIF(req.PrivateKeyWif)
	if err != nil {
		return nil, grpc.Errorf(codes.InvalidArgument,
			"Invalid WIF-encoded private key: %v", err)
	}

	lock := make(chan time.Time, 1)
	defer func() {
		lock <- time.Time{} // send matters, not the value
	}()
	err = s.wallet.Unlock(req.Passphrase, lock)
	if err != nil {
		return nil, translateError(err)
	}

	// At the moment, only the special-cased import account can be used to
	// import keys.
	if req.Account != waddrmgr.ImportedAddrAccount {
		return nil, grpc.Errorf(codes.InvalidArgument,
			"Only the imported account accepts private key imports")
	}

	_, err = s.wallet.ImportPrivateKey(waddrmgr.KeyScopeBIP0044, wif, nil, req.Rescan)
	if err != nil {
		return nil, translateError(err)
	}

	return &pb.ImportPrivateKeyResponse{}, nil
}

func (s *walletServer) Balance(ctx context.Context, req *pb.BalanceRequest) (
	*pb.BalanceResponse, error) {

	account := req.AccountNumber
	reqConfs := req.RequiredConfirmations
	bals, err := s.wallet.CalculateAccountBalances(account, reqConfs)
	if err != nil {
		return nil, translateError(err)
	}

	// TODO: Spendable currently includes multisig outputs that may not
	// actually be spendable without additional keys.
	resp := &pb.BalanceResponse{
		Total:          int64(bals.Total),
		Spendable:      int64(bals.Spendable),
		ImmatureReward: int64(bals.ImmatureReward),
	}
	return resp, nil
}

// confirmed checks whether a transaction at height txHeight has met minconf
// confirmations for a blockchain at height curHeight.
func confirmed(minconf, txHeight, curHeight int32) bool {
	return confirms(txHeight, curHeight) >= minconf
}

// confirms returns the number of confirmations for a transaction in a block at
// height txHeight (or -1 for an unconfirmed tx) given the chain height
// curHeight.
func confirms(txHeight, curHeight int32) int32 {
	switch {
	case txHeight == -1, txHeight > curHeight:
		return 0
	default:
		return curHeight - txHeight + 1
	}
}

func (s *walletServer) FundTransaction(ctx context.Context, req *pb.FundTransactionRequest) (
	*pb.FundTransactionResponse, error) {

	policy := wallet.OutputSelectionPolicy{
		Account:               req.Account,
		RequiredConfirmations: req.RequiredConfirmations,
	}
	unspentOutputs, err := s.wallet.UnspentOutputs(policy)
	if err != nil {
		return nil, translateError(err)
	}

	selectedOutputs := make([]*pb.FundTransactionResponse_PreviousOutput, 0, len(unspentOutputs))
	var totalAmount bchutil.Amount
	for _, output := range unspentOutputs {
		selectedOutputs = append(selectedOutputs, &pb.FundTransactionResponse_PreviousOutput{
			TransactionHash: output.OutPoint.Hash[:],
			OutputIndex:     output.OutPoint.Index,
			Amount:          output.Output.Value,
			PkScript:        output.Output.PkScript,
			ReceiveTime:     output.ReceiveTime.Unix(),
			FromCoinbase:    output.OutputKind == wallet.OutputKindCoinbase,
		})
		totalAmount += bchutil.Amount(output.Output.Value)

		if req.TargetAmount != 0 && totalAmount > bchutil.Amount(req.TargetAmount) {
			break
		}
	}

	var changeScript []byte
	if req.IncludeChangeScript && totalAmount > bchutil.Amount(req.TargetAmount) {
		changeAddr, err := s.wallet.NewChangeAddress(req.Account, waddrmgr.KeyScopeBIP0044)
		if err != nil {
			return nil, translateError(err)
		}
		changeScript, err = txscript.PayToAddrScript(changeAddr)
		if err != nil {
			return nil, translateError(err)
		}
	}

	return &pb.FundTransactionResponse{
		SelectedOutputs: selectedOutputs,
		TotalAmount:     int64(totalAmount),
		ChangePkScript:  changeScript,
	}, nil
}

func (s *walletServer) CreateTransaction(ctx context.Context, req *pb.CreateTransactionRequest) (
	*pb.CreateTransactionResponse, error) {

	fee := bchutil.Amount(req.SatPerKbFee)
	var outputs []*wire.TxOut
	for _, out := range req.Outputs {
		addr, err := bchutil.DecodeAddress(out.Address, s.wallet.ChainParams())
		if err != nil {
			return nil, err
		}
		script, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, wire.NewTxOut(out.Amount, script))
	}

	authoredTx, err := s.wallet.CreateUnsignedTx(req.Account, outputs, req.RequiredConfirmations, fee)
	if err != nil {
		return nil, err
	}
	var serializedTx bytes.Buffer
	err = authoredTx.Tx.BchEncode(&serializedTx, wire.ProtocolVersion, wire.BaseEncoding)
	if err != nil {
		return nil, err
	}

	var inputValues []int64
	totalIn := int64(0)
	for _, val := range authoredTx.PrevInputValues {
		totalIn += int64(val.ToUnit(bchutil.AmountSatoshi))
		inputValues = append(inputValues, int64(val.ToUnit(bchutil.AmountSatoshi)))
	}
	totalOut := int64(0)
	for _, out := range authoredTx.Tx.TxOut {
		totalOut += out.Value
	}

	return &pb.CreateTransactionResponse{
		SerializedTransaction: serializedTx.Bytes(),
		InputValues:           inputValues,
		Fee:                   totalIn - totalOut,
	}, nil
}

func (s *walletServer) SweepAccount(ctx context.Context, req *pb.SweepAccountRequest) (
	*pb.SweepAccountResponse, error) {

	policy := wallet.OutputSelectionPolicy{
		Account:               req.Account,
		RequiredConfirmations: 0,
	}
	unspentOutputs, err := s.wallet.UnspentOutputs(policy)
	if err != nil {
		return nil, translateError(err)
	}

	totalIn := int64(0)
	var inputs []*wire.TxIn
	var inputValues []int64
	for _, u := range unspentOutputs {
		totalIn += u.Output.Value
		inputValues = append(inputValues, u.Output.Value)
		inputs = append(inputs, wire.NewTxIn(&u.OutPoint, nil))
	}

	addr, err := bchutil.DecodeAddress(req.SweepToAddress, s.wallet.ChainParams())
	if err != nil {
		return nil, err
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, err
	}

	// Set the value to zero as a placeholder while we calculate the estimate size
	out := wire.NewTxOut(0, script)
	outputs := []*wire.TxOut{out}

	txSize := txsizes.EstimateSerializeSize(len(inputs), outputs, false)

	fee := (float64(txSize) / float64(1000)) * float64(req.SatPerKbFee)

	out.Value = totalIn - int64(fee)

	tx := &wire.MsgTx{
		Version:  wire.TxVersion,
		TxIn:     inputs,
		TxOut:    outputs,
		LockTime: 0,
	}

	var serializedTx bytes.Buffer
	err = tx.BchEncode(&serializedTx, wire.ProtocolVersion, wire.BaseEncoding)
	if err != nil {
		return nil, err
	}

	return &pb.SweepAccountResponse{
		SerializedTransaction: serializedTx.Bytes(),
		InputValues:           inputValues,
		TotalAmount:           out.Value,
		Fee:                   int64(fee),
	}, nil
}

func marshalGetTransactionsResult(wresp *wallet.GetTransactionsResult) (
	*pb.GetTransactionsResponse, error) {

	resp := &pb.GetTransactionsResponse{
		MinedTransactions:   marshalBlocks(wresp.MinedTransactions),
		UnminedTransactions: marshalTransactionDetails(wresp.UnminedTransactions),
	}
	return resp, nil
}

// BUGS:
// - MinimumRecentTransactions is ignored.
// - Wrong error codes when a block height or hash is not recognized
func (s *walletServer) GetTransactions(ctx context.Context, req *pb.GetTransactionsRequest) (
	resp *pb.GetTransactionsResponse, err error) {

	var startBlock, endBlock *wallet.BlockIdentifier
	if req.StartingBlockHash != nil && req.StartingBlockHeight != 0 {
		return nil, errors.New(
			"starting block hash and height may not be specified simultaneously")
	} else if req.StartingBlockHash != nil {
		startBlockHash, err := chainhash.NewHash(req.StartingBlockHash)
		if err != nil {
			return nil, grpc.Errorf(codes.InvalidArgument, "%s", err.Error())
		}
		startBlock = wallet.NewBlockIdentifierFromHash(startBlockHash)
	} else if req.StartingBlockHeight != 0 {
		startBlock = wallet.NewBlockIdentifierFromHeight(req.StartingBlockHeight)
	}

	if req.EndingBlockHash != nil && req.EndingBlockHeight != 0 {
		return nil, grpc.Errorf(codes.InvalidArgument,
			"ending block hash and height may not be specified simultaneously")
	} else if req.EndingBlockHash != nil {
		endBlockHash, err := chainhash.NewHash(req.EndingBlockHash)
		if err != nil {
			return nil, grpc.Errorf(codes.InvalidArgument, "%s", err.Error())
		}
		endBlock = wallet.NewBlockIdentifierFromHash(endBlockHash)
	} else if req.EndingBlockHeight != 0 {
		endBlock = wallet.NewBlockIdentifierFromHeight(req.EndingBlockHeight)
	}

	var minRecentTxs int
	if req.MinimumRecentTransactions != 0 {
		if endBlock != nil {
			return nil, grpc.Errorf(codes.InvalidArgument,
				"ending block and minimum number of recent transactions "+
					"may not be specified simultaneously")
		}
		minRecentTxs = int(req.MinimumRecentTransactions)
		if minRecentTxs < 0 {
			return nil, grpc.Errorf(codes.InvalidArgument,
				"minimum number of recent transactions may not be negative")
		}
	}

	_ = minRecentTxs

	gtr, err := s.wallet.GetTransactions(startBlock, endBlock, ctx.Done())
	if err != nil {
		return nil, translateError(err)
	}
	return marshalGetTransactionsResult(gtr)
}

func (s *walletServer) ChangePassphrase(ctx context.Context, req *pb.ChangePassphraseRequest) (
	*pb.ChangePassphraseResponse, error) {

	defer func() {
		zero.Bytes(req.OldPassphrase)
		zero.Bytes(req.NewPassphrase)
	}()

	var err error
	switch req.Key {
	case pb.ChangePassphraseRequest_PRIVATE:
		err = s.wallet.ChangePrivatePassphrase(req.OldPassphrase, req.NewPassphrase)
	case pb.ChangePassphraseRequest_PUBLIC:
		err = s.wallet.ChangePublicPassphrase(req.OldPassphrase, req.NewPassphrase)
	default:
		return nil, grpc.Errorf(codes.InvalidArgument, "Unknown key type (%d)", req.Key)
	}
	if err != nil {
		return nil, translateError(err)
	}
	return &pb.ChangePassphraseResponse{}, nil
}

// BUGS:
// - InputIndexes request field is ignored.
func (s *walletServer) SignTransaction(ctx context.Context, req *pb.SignTransactionRequest) (
	*pb.SignTransactionResponse, error) {
	defer zero.Bytes(req.Passphrase)

	var tx wire.MsgTx
	err := tx.Deserialize(bytes.NewReader(req.SerializedTransaction))
	if err != nil {
		return nil, grpc.Errorf(codes.InvalidArgument,
			"Bytes do not represent a valid raw transaction: %v", err)
	}

	lock := make(chan time.Time, 1)
	defer func() {
		lock <- time.Time{} // send matters, not the value
	}()
	err = s.wallet.Unlock(req.Passphrase, lock)
	if err != nil {
		return nil, translateError(err)
	}

	invalidSigs, err := s.wallet.SignTransaction(&tx, req.InputValues, txscript.SigHashAll, nil, nil, nil)
	if err != nil {
		return nil, translateError(err)
	}

	invalidInputIndexes := make([]uint32, len(invalidSigs))
	for i, e := range invalidSigs {
		invalidInputIndexes[i] = e.InputIndex
	}

	var serializedTransaction bytes.Buffer
	serializedTransaction.Grow(tx.SerializeSize())
	err = tx.Serialize(&serializedTransaction)
	if err != nil {
		return nil, translateError(err)
	}

	resp := &pb.SignTransactionResponse{
		Transaction:          serializedTransaction.Bytes(),
		UnsignedInputIndexes: invalidInputIndexes,
	}
	return resp, nil
}

// BUGS:
//   - The transaction is not inspected to be relevant before publishing using
//     sendrawtransaction, so connection errors to bchd could result in the tx
//     never being added to the wallet database.
//   - Once the above bug is fixed, wallet will require a way to purge invalid
//     transactions from the database when they are rejected by the network, other
//     than double spending them.
func (s *walletServer) PublishTransaction(ctx context.Context, req *pb.PublishTransactionRequest) (
	*pb.PublishTransactionResponse, error) {

	var msgTx wire.MsgTx
	err := msgTx.Deserialize(bytes.NewReader(req.SignedTransaction))
	if err != nil {
		return nil, grpc.Errorf(codes.InvalidArgument,
			"Bytes do not represent a valid raw transaction: %v", err)
	}

	err = s.wallet.PublishTransaction(&msgTx)
	if err != nil {
		return nil, translateError(err)
	}
	txid := msgTx.TxHash()
	return &pb.PublishTransactionResponse{Hash: txid[:]}, nil
}

func (s *walletServer) Rescan(ctx context.Context, req *pb.RescanRequest) (
	*pb.RescanResponse, error) {

	job, err := s.wallet.NewRescanJob()
	if err != nil {
		return nil, err
	}
	s.wallet.SubmitRescan(job)
	return &pb.RescanResponse{}, nil
}

func (s *walletServer) DownloadPaymentRequest(ctx context.Context, req *pb.DownloadPaymentRequestRequest) (
	*pb.DownloadPaymentRequestResponse, error) {

	client := pymtproto.NewPaymentProtocolClient(s.wallet.ChainParams(), s.wallet.GetProxyDialer())
	pr, err := client.DownloadBip0070PaymentRequest(req.Uri)
	if err != nil {
		return nil, err
	}
	resp := &pb.DownloadPaymentRequestResponse{
		PayToName:    pr.PayToName,
		Expires:      pr.Expires.Unix(),
		Memo:         pr.Memo,
		PaymentUrl:   pr.PaymentURL,
		MerchantData: pr.MerchantData,
	}
	for _, out := range pr.Outputs {
		output := &pb.DownloadPaymentRequestResponse_Output{
			Amount:  int64(out.Amount.ToUnit(bchutil.AmountSatoshi)),
			Address: out.Address.String(),
		}
		resp.Outputs = append(resp.Outputs, output)
	}
	return resp, nil
}

func (s *walletServer) PostPayment(ctx context.Context, req *pb.PostPaymentRequest) (
	*pb.PostPaymentResponse, error) {

	client := pymtproto.NewPaymentProtocolClient(s.wallet.ChainParams(), s.wallet.GetProxyDialer())

	refundAddr, err := bchutil.DecodeAddress(req.RefundOutput.Address, s.wallet.ChainParams())
	if err != nil {
		return nil, err
	}

	payment := &pymtproto.Payment{
		PaymentURL:   req.PaymentUrl,
		MerchantData: req.MerchantData,
		Memo:         req.Memo,
		RefundOutput: pymtproto.Output{
			Amount:  bchutil.Amount(req.RefundOutput.Amount),
			Address: refundAddr,
		},
	}

	for _, serializedTx := range req.Transactions {
		tx := &wire.MsgTx{}
		if err := tx.BchDecode(bytes.NewReader(serializedTx), wire.ProtocolVersion, wire.BaseEncoding); err != nil {
			return nil, err
		}
		payment.Transactions = append(payment.Transactions, tx)
	}

	ackMemo, err := client.PostPayment(payment)
	if err != nil {
		return nil, err
	}
	resp := &pb.PostPaymentResponse{
		Memo: ackMemo,
	}
	return resp, nil
}

func (s *walletServer) ValidateAddress(ctx context.Context, req *pb.ValidateAddressRequest) (
	*pb.ValidateAddressResponse, error) {

	valid := false
	_, err := bchutil.DecodeAddress(req.Address, s.wallet.ChainParams())
	if err == nil {
		valid = true
	}
	return &pb.ValidateAddressResponse{Valid: valid}, nil
}

func marshalTransactionInputs(v []wallet.TransactionSummaryInput) []*pb.TransactionDetails_Input {
	inputs := make([]*pb.TransactionDetails_Input, len(v))
	for i := range v {
		input := &v[i]
		inputs[i] = &pb.TransactionDetails_Input{
			Index:           input.Index,
			PreviousAccount: input.PreviousAccount,
			PreviousAmount:  int64(input.PreviousAmount),
		}
	}
	return inputs
}

func marshalTransactionOutputs(v []wallet.TransactionSummaryOutput) []*pb.TransactionDetails_Output {
	outputs := make([]*pb.TransactionDetails_Output, len(v))
	for i := range v {
		output := &v[i]
		var address string
		if output.Address != nil {
			address = output.Address.String()
		}
		outputs[i] = &pb.TransactionDetails_Output{
			Index:    output.Index,
			Account:  output.Account,
			Internal: output.Internal,
			Address:  address,
			Amount:   int64(output.Amount.ToUnit(bchutil.AmountSatoshi)),
		}
	}
	return outputs
}

func marshalTransactionDetails(v []wallet.TransactionSummary) []*pb.TransactionDetails {
	txs := make([]*pb.TransactionDetails, len(v))
	for i := range v {
		tx := &v[i]
		txs[i] = &pb.TransactionDetails{
			Hash:        tx.Hash[:],
			Transaction: tx.Transaction,
			Debits:      marshalTransactionInputs(tx.MyInputs),
			Credits:     marshalTransactionOutputs(tx.MyOutputs),
			Fee:         int64(tx.Fee),
			Timestamp:   tx.Timestamp,
		}
	}
	return txs
}

func marshalBlocks(v []wallet.Block) []*pb.BlockDetails {
	blocks := make([]*pb.BlockDetails, len(v))
	for i := range v {
		block := &v[i]
		blocks[i] = &pb.BlockDetails{
			Hash:         block.Hash[:],
			Height:       block.Height,
			Timestamp:    block.Timestamp,
			Transactions: marshalTransactionDetails(block.Transactions),
		}
	}
	return blocks
}

func marshalHashes(v []*chainhash.Hash) [][]byte {
	hashes := make([][]byte, len(v))
	for i, hash := range v {
		hashes[i] = hash[:]
	}
	return hashes
}

func marshalAccountBalances(v []wallet.AccountBalance) []*pb.AccountBalance {
	balances := make([]*pb.AccountBalance, len(v))
	for i := range v {
		balance := &v[i]
		balances[i] = &pb.AccountBalance{
			Account:      balance.Account,
			TotalBalance: int64(balance.TotalBalance),
		}
	}
	return balances
}

func (s *walletServer) TransactionNotifications(req *pb.TransactionNotificationsRequest,
	svr pb.WalletService_TransactionNotificationsServer) error {

	n := s.wallet.NtfnServer.TransactionNotifications()
	defer n.Done()

	ctxDone := svr.Context().Done()
	for {
		select {
		case v := <-n.C:
			resp := pb.TransactionNotificationsResponse{
				AttachedBlocks:           marshalBlocks(v.AttachedBlocks),
				DetachedBlocks:           marshalHashes(v.DetachedBlocks),
				UnminedTransactions:      marshalTransactionDetails(v.UnminedTransactions),
				UnminedTransactionHashes: marshalHashes(v.UnminedTransactionHashes),
			}
			err := svr.Send(&resp)
			if err != nil {
				return translateError(err)
			}

		case <-ctxDone:
			return nil
		}
	}
}

func (s *walletServer) SpentnessNotifications(req *pb.SpentnessNotificationsRequest,
	svr pb.WalletService_SpentnessNotificationsServer) error {

	if req.NoNotifyUnspent && req.NoNotifySpent {
		return grpc.Errorf(codes.InvalidArgument,
			"no_notify_unspent and no_notify_spent may not both be true")
	}

	n := s.wallet.NtfnServer.AccountSpentnessNotifications(req.Account)
	defer n.Done()

	ctxDone := svr.Context().Done()
	for {
		select {
		case v := <-n.C:
			spenderHash, spenderIndex, spent := v.Spender()
			if (spent && req.NoNotifySpent) || (!spent && req.NoNotifyUnspent) {
				continue
			}
			index := v.Index()
			resp := pb.SpentnessNotificationsResponse{
				TransactionHash: v.Hash()[:],
				OutputIndex:     index,
			}
			if spent {
				resp.Spender = &pb.SpentnessNotificationsResponse_Spender{
					TransactionHash: spenderHash[:],
					InputIndex:      spenderIndex,
				}
			}
			err := svr.Send(&resp)
			if err != nil {
				return translateError(err)
			}

		case <-ctxDone:
			return nil
		}
	}
}

func (s *walletServer) AccountNotifications(req *pb.AccountNotificationsRequest,
	svr pb.WalletService_AccountNotificationsServer) error {

	n := s.wallet.NtfnServer.AccountNotifications()
	defer n.Done()

	ctxDone := svr.Context().Done()
	for {
		select {
		case v := <-n.C:
			resp := pb.AccountNotificationsResponse{
				AccountNumber:    v.AccountNumber,
				AccountName:      v.AccountName,
				ExternalKeyCount: v.ExternalKeyCount,
				InternalKeyCount: v.InternalKeyCount,
				ImportedKeyCount: v.ImportedKeyCount,
			}
			err := svr.Send(&resp)
			if err != nil {
				return translateError(err)
			}

		case <-ctxDone:
			return nil
		}
	}
}

func (s *walletServer) RescanNotifications(req *pb.RescanNotificationsRequest,
	svr pb.WalletService_RescanNotificationsServer) error {

	n := s.wallet.NtfnServer.RescanNotifications()
	defer n.Done()

	ctxDone := svr.Context().Done()
	for {
		select {
		case v := <-n.C:
			resp := pb.RescanNotificationsResponse{
				Hash:     v.Hash[:],
				Height:   v.Height,
				Finished: v.Finished,
			}
			err := svr.Send(&resp)
			if err != nil {
				return translateError(err)
			}

		case <-ctxDone:
			return nil
		}
	}
}

// StartWalletLoaderService creates an implementation of the WalletLoaderService
// and registers it with the gRPC server.
func StartWalletLoaderService(server *grpc.Server, loader *wallet.Loader, activeNet *netparams.Params) {
	loaderService.loader = loader
	loaderService.activeNet = activeNet
	if atomic.SwapUint32(&loaderService.ready, 1) != 0 {
		panic("service already started")
	}
}

func (s *loaderServer) checkReady() bool {
	return atomic.LoadUint32(&s.ready) != 0
}

func (s *loaderServer) CreateWallet(ctx context.Context, req *pb.CreateWalletRequest) (
	*pb.CreateWalletResponse, error) {

	seed := bip39.NewSeed(req.MnemonicSeed, "")

	defer func() {
		zero.Bytes(req.PrivatePassphrase)
		zero.Bytes(seed)
		req.WalletBirthday = 0
		req.MnemonicSeed = ""
	}()

	// Use an insecure public passphrase when the request's is empty.
	pubPassphrase := req.PublicPassphrase
	if len(pubPassphrase) == 0 {
		pubPassphrase = []byte(wallet.InsecurePubPassphrase)
	}

	wallet, err := s.loader.CreateNewWallet(
		pubPassphrase, req.PrivatePassphrase, seed, time.Unix(req.WalletBirthday, 0),
	)
	if err != nil {
		return nil, translateError(err)
	}

	s.mu.Lock()
	if s.rpcClient != nil {
		wallet.SynchronizeRPC(s.rpcClient)
	}
	s.mu.Unlock()

	return &pb.CreateWalletResponse{}, nil
}

func (s *loaderServer) OpenWallet(ctx context.Context, req *pb.OpenWalletRequest) (
	*pb.OpenWalletResponse, error) {

	// Use an insecure public passphrase when the request's is empty.
	pubPassphrase := req.PublicPassphrase
	if len(pubPassphrase) == 0 {
		pubPassphrase = []byte(wallet.InsecurePubPassphrase)
	}

	wallet, err := s.loader.OpenExistingWallet(pubPassphrase, false)
	if err != nil {
		return nil, translateError(err)
	}

	s.mu.Lock()
	if s.rpcClient != nil {
		wallet.SynchronizeRPC(s.rpcClient)
	}
	s.mu.Unlock()

	return &pb.OpenWalletResponse{}, nil
}

func (s *loaderServer) WalletExists(ctx context.Context, req *pb.WalletExistsRequest) (
	*pb.WalletExistsResponse, error) {

	exists, err := s.loader.WalletExists()
	if err != nil {
		return nil, translateError(err)
	}
	return &pb.WalletExistsResponse{Exists: exists}, nil
}

func (s *loaderServer) CloseWallet(ctx context.Context, req *pb.CloseWalletRequest) (
	*pb.CloseWalletResponse, error) {

	err := s.loader.UnloadWallet()
	if err == wallet.ErrNotLoaded {
		return nil, grpc.Errorf(codes.FailedPrecondition, "wallet is not loaded")
	}
	if err != nil {
		return nil, translateError(err)
	}

	return &pb.CloseWalletResponse{}, nil
}

func (s *loaderServer) StartConsensusRPC(ctx context.Context, req *pb.StartConsensusRpcRequest) (
	*pb.StartConsensusRpcResponse, error) {

	defer zero.Bytes(req.Password)

	defer s.mu.Unlock()
	s.mu.Lock()

	if s.rpcClient != nil {
		return nil, grpc.Errorf(codes.FailedPrecondition, "RPC client already created")
	}

	networkAddress, err := cfgutil.NormalizeAddress(req.NetworkAddress,
		s.activeNet.RPCClientPort)
	if err != nil {
		return nil, grpc.Errorf(codes.InvalidArgument,
			"Network address is ill-formed: %v", err)
	}

	// Error if the wallet is already syncing with the network.
	wallet, walletLoaded := s.loader.LoadedWallet()
	if walletLoaded && wallet.SynchronizingToNetwork() {
		return nil, grpc.Errorf(codes.FailedPrecondition,
			"wallet is loaded and already synchronizing")
	}

	rpcClient, err := chain.NewRPCClient(s.activeNet.Params, networkAddress, req.Username,
		string(req.Password), req.Certificate, len(req.Certificate) == 0, 1)
	if err != nil {
		return nil, translateError(err)
	}

	err = rpcClient.Start()
	if err != nil {
		if err == rpcclient.ErrInvalidAuth {
			return nil, grpc.Errorf(codes.InvalidArgument,
				"Invalid RPC credentials: %v", err)
		}
		return nil, grpc.Errorf(codes.NotFound,
			"Connection to RPC server failed: %v", err)
	}

	s.rpcClient = rpcClient

	if walletLoaded {
		wallet.SynchronizeRPC(rpcClient)
	}

	return &pb.StartConsensusRpcResponse{}, nil
}

func (s *loaderServer) GenerateMnemonicSeed(ctx context.Context, req *pb.GenerateMnemonicSeedRequest) (
	*pb.GenerateMnemonicSeedResponse, error) {

	ent, err := bip39.NewEntropy(int(req.BitSize))
	if err != nil {
		return nil, err
	}
	mnemonic, err := bip39.NewMnemonic(ent)
	if err != nil {
		return nil, err
	}
	return &pb.GenerateMnemonicSeedResponse{Mnemonic: mnemonic}, nil
}
