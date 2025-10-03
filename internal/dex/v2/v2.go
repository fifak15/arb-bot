package v2

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/you/arb-bot/internal/dex/core"
)

const routerABI = `[
 {"inputs":[{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"address[]","name":"path","type":"address[]"}],"name":"getAmountsOut","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],"stateMutability":"view","type":"function"},
 {"inputs":[{"internalType":"uint256","name":"amountOut","type":"uint256"},{"internalType":"address[]","name":"path","type":"address[]"}],"name":"getAmountsIn","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],"stateMutability":"view","type":"function"},
 {"inputs":[{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"uint256","name":"amountOutMin","type":"uint256"},{"internalType":"address[]","name":"path","type":"address[]"},{"internalType":"address","name":"to","type":"address"},{"internalType":"uint256","name":"deadline","type":"uint256"}],"name":"swapExactTokensForTokens","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],"stateMutability":"nonpayable","type":"function"},
 {"inputs":[{"internalType":"uint256","name":"amountOut","type":"uint256"},{"internalType":"uint256","name":"amountInMax","type":"uint256"},{"internalType":"address[]","name":"path","type":"address[]"},{"internalType":"address","name":"to","type":"address"},{"internalType":"uint256","name":"deadline","type":"uint256"}],"name":"swapTokensForExactTokens","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],"stateMutability":"nonpayable","type":"function"}
]`

const erc20ABI = `[
 {"inputs":[],"name":"decimals","outputs":[{"internalType":"uint8","name":"","type":"uint8"}],"stateMutability":"view","type":"function"}
]`

type V2 struct {
	ec        *ethclient.Client
	abi       abi.ABI
	erc20ABI  abi.ABI
	router    common.Address
	recipient common.Address
	gasLimit  uint64
	priv      *ecdsa.PrivateKey
	from      common.Address
	chainID   *big.Int
	decMu     sync.RWMutex
	decimals  map[common.Address]int
}

func New(ec *ethclient.Client, router, recipient common.Address, gasLimit uint64, pkHex string) (*V2, error) {
	rABI, err := abi.JSON(strings.NewReader(routerABI))
	if err != nil {
		return nil, err
	}
	eABI, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		return nil, err
	}
	var (
		key     *ecdsa.PrivateKey
		from    common.Address
		chainID *big.Int
	)
	if strings.TrimSpace(pkHex) != "" {
		key, err = crypto.HexToECDSA(strings.TrimPrefix(pkHex, "0x"))
		if err != nil {
			return nil, err
		}
		from = crypto.PubkeyToAddress(key.PublicKey)
		chainID, err = ec.ChainID(context.Background())
		if err != nil {
			return nil, err
		}
	}

	if gasLimit == 0 {
		gasLimit = 400_000
	}

	return &V2{
		ec:        ec,
		abi:       rABI,
		erc20ABI:  eABI,
		router:    router,
		recipient: recipient,
		gasLimit:  gasLimit,
		priv:      key,
		from:      from,
		chainID:   chainID,
		decimals:  make(map[common.Address]int, 8),
	}, nil
}

// ---------- core.Quoter ----------

func (v *V2) QuoteDexOutUSD(ctx context.Context, tokenIn, tokenOut common.Address, qtyBase, ethUSD float64) (outUSD, gasUSD float64, meta core.QuoteMeta, err error) {
	inWei, outDec, err := v.calcInWeiAndOutDecimals(ctx, tokenIn, tokenOut, qtyBase)
	if err != nil {
		return 0, 0, core.QuoteMeta{}, err
	}

	path := []common.Address{tokenIn, tokenOut}
	data, _ := v.abi.Pack("getAmountsOut", inWei, path)
	raw, err := v.ec.CallContract(ctx, ethereum.CallMsg{To: &v.router, Data: data}, nil)
	if err != nil {
		return 0, 0, core.QuoteMeta{}, err
	}
	outs, err := v.abi.Methods["getAmountsOut"].Outputs.Unpack(raw)
	if err != nil || len(outs) == 0 {
		return 0, 0, core.QuoteMeta{}, errors.New("decode getAmountsOut")
	}
	amounts := outs[0].([]*big.Int)
	if len(amounts) < 2 {
		return 0, 0, core.QuoteMeta{}, errors.New("bad amounts length")
	}
	outFloat := toFloat(amounts[len(amounts)-1], outDec)
	outUSD = outFloat

	deadline := big.NewInt(time.Now().Add(5 * time.Minute).Unix())
	swapData, _ := v.abi.Pack("swapExactTokensForTokens", inWei, big.NewInt(0), path, v.recipient, deadline)
	gasUSD, _ = v.estimateGasUSD(ctx, swapData, ethUSD)
	return outUSD, gasUSD, core.QuoteMeta{}, nil
}

func (v *V2) QuoteDexInUSD(ctx context.Context, tokenIn, tokenOut common.Address, qtyBase, ethUSD float64) (inUSD, gasUSD float64, meta core.QuoteMeta, err error) {
	outWei, inDec, err := v.calcOutWeiAndInDecimals(ctx, tokenIn, tokenOut, qtyBase)
	if err != nil {
		return 0, 0, core.QuoteMeta{}, err
	}

	path := []common.Address{tokenIn, tokenOut}
	data, _ := v.abi.Pack("getAmountsIn", outWei, path)
	raw, err := v.ec.CallContract(ctx, ethereum.CallMsg{To: &v.router, Data: data}, nil)
	if err != nil {
		return 0, 0, core.QuoteMeta{}, err
	}
	outs, err := v.abi.Methods["getAmountsIn"].Outputs.Unpack(raw)
	if err != nil || len(outs) == 0 {
		return 0, 0, core.QuoteMeta{}, errors.New("decode getAmountsIn")
	}
	amounts := outs[0].([]*big.Int)
	if len(amounts) < 2 {
		return 0, 0, core.QuoteMeta{}, errors.New("bad amounts length")
	}
	inFloat := toFloat(amounts[0], inDec)
	inUSD = inFloat

	inWei := new(big.Int).Set(amounts[0])
	maxIn := new(big.Int).Mul(inWei, big.NewInt(102))
	maxIn = new(big.Int).Div(maxIn, big.NewInt(100))
	deadline := big.NewInt(time.Now().Add(5 * time.Minute).Unix())
	swapData, _ := v.abi.Pack("swapTokensForExactTokens", outWei, maxIn, path, v.recipient, deadline)
	gasUSD, _ = v.estimateGasUSD(ctx, swapData, ethUSD)

	return inUSD, gasUSD, core.QuoteMeta{}, nil
}

func (v *V2) SwapExactInput(ctx context.Context, tokenIn, tokenOut common.Address, amountIn, minOut *big.Int, _ core.QuoteMeta) (string, error) {
	if v.priv == nil {
		return "", errors.New("v2 router: no private key configured")
	}
	path := []common.Address{tokenIn, tokenOut}
	deadline := big.NewInt(time.Now().Add(5 * time.Minute).Unix())
	data, _ := v.abi.Pack("swapExactTokensForTokens", amountIn, minOut, path, v.recipient, deadline)
	return v.sendSwap(ctx, data)
}

func (v *V2) SwapExactOutput(ctx context.Context, tokenIn, tokenOut common.Address, amountOut, maxIn *big.Int, _ core.QuoteMeta) (string, error) {
	if v.priv == nil {
		return "", errors.New("v2 router: no private key configured")
	}
	path := []common.Address{tokenIn, tokenOut}
	deadline := big.NewInt(time.Now().Add(5 * time.Minute).Unix())
	data, _ := v.abi.Pack("swapTokensForExactTokens", amountOut, maxIn, path, v.recipient, deadline)
	return v.sendSwap(ctx, data)
}

// ---------- helpers ----------

func (v *V2) calcInWeiAndOutDecimals(ctx context.Context, tokenIn, tokenOut common.Address, qtyBase float64) (*big.Int, int, error) {
	inDec, err := v.fetchDecimals(ctx, tokenIn)
	if err != nil {
		return nil, 0, err
	}
	outDec, err := v.fetchDecimals(ctx, tokenOut)
	if err != nil {
		return nil, 0, err
	}
	scaleIn := big.NewFloat(math.Pow10(inDec))
	inWeiF := new(big.Float).Mul(big.NewFloat(qtyBase), scaleIn)
	inWei := new(big.Int)
	inWeiF.Int(inWei)
	return inWei, outDec, nil
}

func (v *V2) calcOutWeiAndInDecimals(ctx context.Context, tokenIn, tokenOut common.Address, qtyBase float64) (*big.Int, int, error) {
	outDec, err := v.fetchDecimals(ctx, tokenOut)
	if err != nil {
		return nil, 0, err
	}
	inDec, err := v.fetchDecimals(ctx, tokenIn)
	if err != nil {
		return nil, 0, err
	}
	scaleOut := big.NewFloat(math.Pow10(outDec))
	outWeiF := new(big.Float).Mul(big.NewFloat(qtyBase), scaleOut)
	outWei := new(big.Int)
	outWeiF.Int(outWei)
	return outWei, inDec, nil
}

func (v *V2) fetchDecimals(ctx context.Context, token common.Address) (int, error) {
	v.decMu.RLock()
	if d, ok := v.decimals[token]; ok {
		v.decMu.RUnlock()
		return d, nil
	}
	v.decMu.RUnlock()

	data, _ := v.erc20ABI.Pack("decimals")
	raw, err := v.ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return 0, err
	}
	outs, err := v.erc20ABI.Methods["decimals"].Outputs.Unpack(raw)
	if err != nil || len(outs) == 0 {
		return 0, errors.New("decode decimals")
	}
	var d int
	switch x := outs[0].(type) {
	case uint8:
		d = int(x)
	case *big.Int:
		d = int(x.Int64())
	default:
		return 0, errors.New("unexpected decimals type")
	}
	v.decMu.Lock()
	v.decimals[token] = d
	v.decMu.Unlock()
	return d, nil
}

func (v *V2) estimateGasUSD(ctx context.Context, data []byte, ethUSD float64) (float64, error) {
	msg := ethereum.CallMsg{From: v.from, To: &v.router, Data: data}
	gas, err := v.ec.EstimateGas(ctx, msg)
	if err != nil || gas == 0 {
		gas = v.gasLimit
	}

	tip, _ := v.ec.SuggestGasTipCap(ctx)
	if tip == nil {
		tip = big.NewInt(2_000_000_000)
	}
	var baseFee *big.Int
	if h, _ := v.ec.HeaderByNumber(ctx, nil); h != nil && h.BaseFee != nil {
		baseFee = new(big.Int).Set(h.BaseFee)
	} else {
		if sp, _ := v.ec.SuggestGasPrice(ctx); sp != nil {
			baseFee = sp
		} else {
			baseFee = big.NewInt(5_000_000_000)
		}
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)

	totalWei := new(big.Int).Mul(feeCap, new(big.Int).SetUint64(gas))
	ethFloat := new(big.Float).Quo(new(big.Float).SetInt(totalWei), big.NewFloat(1e18))
	ethVal, _ := ethFloat.Float64()
	gasUSD := ethVal * ethUSD

	if !isFinite(gasUSD) || gasUSD < 0 {
		return 0, errors.New("bad gasUSD")
	}
	return gasUSD, nil
}

func (v *V2) sendSwap(ctx context.Context, data []byte) (string, error) {
	tip, err := v.ec.SuggestGasTipCap(ctx)
	if err != nil || tip == nil {
		tip = big.NewInt(2_000_000_000)
	}
	var baseFee *big.Int
	if h, _ := v.ec.HeaderByNumber(ctx, nil); h != nil && h.BaseFee != nil {
		baseFee = h.BaseFee
	} else {
		if sp, _ := v.ec.SuggestGasPrice(ctx); sp != nil {
			baseFee = sp
		} else {
			baseFee = big.NewInt(5_000_000_000)
		}
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)

	nonce, err := v.ec.PendingNonceAt(ctx, v.from)
	if err != nil {
		return "", err
	}

	// Gas estimate с фоллбеком
	gas, err := v.ec.EstimateGas(ctx, ethereum.CallMsg{From: v.from, To: &v.router, Data: data})
	if err != nil || gas == 0 {
		gas = v.gasLimit
	}

	tx := gethtypes.NewTx(&gethtypes.DynamicFeeTx{
		ChainID:   v.chainID,
		Nonce:     nonce,
		To:        &v.router,
		Gas:       gas,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Data:      data,
		Value:     big.NewInt(0),
	})
	signed, err := gethtypes.SignTx(tx, gethtypes.LatestSignerForChainID(v.chainID), v.priv)
	if err != nil {
		return "", err
	}
	if err := v.ec.SendTransaction(ctx, signed); err != nil {
		return "", err
	}
	return signed.Hash().Hex(), nil
}

// toFloat converts wei (scaled int) to decimal float using token decimals.
func toFloat(x *big.Int, decimals int) float64 {
	if x == nil {
		return 0
	}
	f := new(big.Float).SetInt(x)
	div := new(big.Float).SetFloat64(math.Pow10(decimals))
	f.Quo(f, div)
	val, _ := f.Float64()
	return val
}

func isFinite(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 1) && !math.IsInf(x, -1)
}
