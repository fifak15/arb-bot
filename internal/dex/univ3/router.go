package univ3

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/you/arb-bot/internal/config"
	"go.uber.org/zap"
)

// Official V3 router address
const v3RouterAddr = "0xE592427A0AEce92De3Edee1F18E0157C05861564"

// Minimal ABI for Router.exactInputSingle and exactOutputSingle
const routerABI = `[
    {"inputs":[{"components":[{"internalType":"address","name":"tokenIn","type":"address"},{"internalType":"address","name":"tokenOut","type":"address"},{"internalType":"uint24","name":"fee","type":"uint24"},{"internalType":"address","name":"recipient","type":"address"},{"internalType":"uint256","name":"deadline","type":"uint256"},{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"uint256","name":"amountOutMinimum","type":"uint256"},{"internalType":"uint160","name":"sqrtPriceLimitX96","type":"uint160"}],"internalType":"struct ISwapRouter.ExactInputSingleParams","name":"params","type":"tuple"}],"name":"exactInputSingle","outputs":[{"internalType":"uint256","name":"amountOut","type":"uint256"}],"stateMutability":"payable","type":"function"},
    {"inputs":[{"components":[{"internalType":"address","name":"tokenIn","type":"address"},{"internalType":"address","name":"tokenOut","type":"address"},{"internalType":"uint24","name":"fee","type":"uint24"},{"internalType":"address","name":"recipient","type":"address"},{"internalType":"uint256","name":"deadline","type":"uint256"},{"internalType":"uint256","name":"amountOut","type":"uint256"},{"internalType":"uint256","name":"amountInMaximum","type":"uint256"},{"internalType":"uint160","name":"sqrtPriceLimitX96","type":"uint160"}],"internalType":"struct ISwapRouter.ExactOutputSingleParams","name":"params","type":"tuple"}],"name":"exactOutputSingle","outputs":[{"internalType":"uint256","name":"amountIn","type":"uint256"}],"stateMutability":"payable","type":"function"}
]`

type onchainRouter struct {
	cfg    *config.Config
	log    *zap.Logger
	ec     *ethclient.Client
	rabi   abi.ABI
	pk     *ecdsa.PrivateKey
	sender common.Address
}

func NewRouter(cfg *config.Config, log *zap.Logger) (Router, error) {
	ec, err := ethclient.Dial(cfg.Chain.RPCHTTP)
	if err != nil {
		return nil, fmt.Errorf("dial rpc: %w", err)
	}

	rabi, err := abi.JSON(strings.NewReader(routerABI))
	if err != nil {
		return nil, fmt.Errorf("parse router abi: %w", err)
	}

	pk, err := crypto.HexToECDSA(cfg.Chain.WalletPK)
	if err != nil {
		return nil, fmt.Errorf("bad private key: %w", err)
	}

	return &onchainRouter{
		cfg:    cfg,
		log:    log,
		ec:     ec,
		rabi:   rabi,
		pk:     pk,
		sender: crypto.PubkeyToAddress(pk.PublicKey),
	}, nil
}

func (r *onchainRouter) SwapExactInput(ctx context.Context, tokenIn, tokenOut common.Address, amountIn *big.Int, minOut *big.Int, feeTier uint32) (txHash string, err error) {
	deadline := time.Now().Add(2 * time.Minute).Unix()
	params := struct {
		TokenIn           common.Address
		TokenOut          common.Address
		Fee               *big.Int
		Recipient         common.Address
		Deadline          *big.Int
		AmountIn          *big.Int
		AmountOutMinimum  *big.Int
		SqrtPriceLimitX96 *big.Int
	}{
		TokenIn:           tokenIn,
		TokenOut:          tokenOut,
		Fee:               big.NewInt(int64(feeTier)),
		Recipient:         r.sender,
		Deadline:          big.NewInt(deadline),
		AmountIn:          amountIn,
		AmountOutMinimum:  minOut,
		SqrtPriceLimitX96: big.NewInt(0),
	}

	input, err := r.rabi.Pack("exactInputSingle", params)
	if err != nil {
		return "", fmt.Errorf("pack exactInputSingle: %w", err)
	}

	signedTx, err := r.signTx(ctx, input)
	if err != nil {
		return "", fmt.Errorf("sign tx: %w", err)
	}

	err = r.ec.SendTransaction(ctx, signedTx)
	if err != nil {
		return "", fmt.Errorf("send transaction: %w", err)
	}
	return signedTx.Hash().Hex(), nil
}

func (r *onchainRouter) SwapExactOutput(ctx context.Context, tokenIn, tokenOut common.Address, amountOut *big.Int, maxIn *big.Int, feeTier uint32) (txHash string, err error) {
	deadline := time.Now().Add(2 * time.Minute).Unix()
	params := struct {
		TokenIn           common.Address
		TokenOut          common.Address
		Fee               *big.Int
		Recipient         common.Address
		Deadline          *big.Int
		AmountOut         *big.Int
		AmountInMaximum   *big.Int
		SqrtPriceLimitX96 *big.Int
	}{
		TokenIn:           tokenIn,
		TokenOut:          tokenOut,
		Fee:               big.NewInt(int64(feeTier)),
		Recipient:         r.sender,
		Deadline:          big.NewInt(deadline),
		AmountOut:         amountOut,
		AmountInMaximum:   maxIn,
		SqrtPriceLimitX96: big.NewInt(0),
	}

	input, err := r.rabi.Pack("exactOutputSingle", params)
	if err != nil {
		return "", fmt.Errorf("pack exactOutputSingle: %w", err)
	}

	signedTx, err := r.signTx(ctx, input)
	if err != nil {
		return "", fmt.Errorf("sign tx: %w", err)
	}

	err = r.ec.SendTransaction(ctx, signedTx)
	if err != nil {
		return "", fmt.Errorf("send transaction: %w", err)
	}
	return signedTx.Hash().Hex(), nil
}

func (r *onchainRouter) signTx(ctx context.Context, input []byte) (*types.Transaction, error) {
	chainID, err := r.ec.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get chain id: %w", err)
	}

	nonce, err := r.ec.PendingNonceAt(ctx, r.sender)
	if err != nil {
		return nil, fmt.Errorf("get nonce: %w", err)
	}

	gasTipCap, err := r.ec.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, fmt.Errorf("suggest gas tip cap: %w", err)
	}

	header, err := r.ec.HeaderByNumber(ctx, nil)
	if err != nil || header.BaseFee == nil {
		return nil, fmt.Errorf("get header/base fee: %w", err)
	}
	gasFeeCap := new(big.Int).Add(
		new(big.Int).Mul(header.BaseFee, big.NewInt(2)),
		gasTipCap,
	)

	routerAddr := common.HexToAddress(v3RouterAddr)
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: gasTipCap,
		GasFeeCap: gasFeeCap,
		Gas:       r.cfg.Chain.GasLimitSwap,
		To:        &routerAddr,
		Value:     big.NewInt(0),
		Data:      input,
	})

	signedTx, err := types.SignTx(tx, types.NewLondonSigner(chainID), r.pk)
	if err != nil {
		return nil, fmt.Errorf("sign transaction: %w", err)
	}

	return signedTx, nil
}