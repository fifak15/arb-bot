package univ3

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/you/arb-bot/internal/config"
	"go.uber.org/zap"
)

type Quoter interface {
	QuoteDexOutUSD(ctx context.Context, tokenIn, tokenOut common.Address, qtyBase float64, ethUSD float64) (outUSD float64, gasUSD float64, feeTier uint32, err error)
	QuoteDexInUSD(ctx context.Context, tokenIn, tokenOut common.Address, qtyBase float64, ethUSD float64) (inUSD float64, gasUSD float64, feeTier uint32, err error)
}

type Router interface {
	SwapExactInput(ctx context.Context, tokenIn, tokenOut common.Address, amountIn *big.Int, minOut *big.Int, feeTier uint32) (txHash string, err error)
	SwapExactOutput(ctx context.Context, tokenIn, tokenOut common.Address, amountOut *big.Int, maxIn *big.Int, feeTier uint32) (txHash string, err error)
}

const (
	Multicall3Address = "0xcA11bde05977b3631167028862bE2a173976CA11"
	multicallABI      = `[{"inputs":[{"components":[{"internalType":"address","name":"target","type":"address"},{"internalType":"bool","name":"allowFailure","type":"bool"},{"internalType":"bytes","name":"callData","type":"bytes"}],"internalType":"struct Multicall3.Call3[]","name":"calls","type":"tuple[]"}],"name":"aggregate3","outputs":[{"components":[{"internalType":"bool","name":"success","type":"bool"},{"internalType":"bytes","name":"returnData","type":"bytes"}],"internalType":"struct Multicall3.Result[]","name":"returnData","type":"tuple[]"}],"stateMutability":"view","type":"function"}]`
)

// MultiQuoter uses the Multicall3 contract to batch multiple quote requests into a single RPC call.
type MultiQuoter struct {
	cfg      *config.Config
	log      *zap.Logger
	ec       *ethclient.Client
	eabi     abi.ABI
	q2abi    abi.ABI
	multiabi abi.ABI
	decMu    sync.RWMutex
	decCache map[common.Address]int
}

const erc20ABI = `[
  {"inputs":[],"name":"decimals","outputs":[{"internalType":"uint8","name":"","type":"uint8"}],"stateMutability":"view","type":"function"}
]`

const quoterV2ABI = `[
  {"inputs":[{"components":[{"internalType":"address","name":"tokenIn","type":"address"},{"internalType":"address","name":"tokenOut","type":"address"},{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"uint24","name":"fee","type":"uint24"},{"internalType":"uint160","name":"sqrtPriceLimitX96","type":"uint160"}],"internalType":"struct IQuoterV2.QuoteExactInputSingleParams","name":"params","type":"tuple"}],"name":"quoteExactInputSingle","outputs":[{"internalType":"uint256","name":"amountOut","type":"uint256"},{"internalType":"uint160","name":"sqrtPriceX96After","type":"uint160"},{"internalType":"uint32","name":"initializedTicksCrossed","type":"uint32"},{"internalType":"uint256","name":"gasEstimate","type":"uint256"}],"stateMutability":"nonpayable","type":"function"},
  {"inputs":[{"components":[{"internalType":"address","name":"tokenIn","type":"address"},{"internalType":"address","name":"tokenOut","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"},{"internalType":"uint24","name":"fee","type":"uint24"},{"internalType":"uint160","name":"sqrtPriceLimitX96","type":"uint160"}],"internalType":"struct IQuoterV2.QuoteExactOutputSingleParams","name":"params","type":"tuple"}],"name":"quoteExactOutputSingle","outputs":[{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"uint160","name":"sqrtPriceX96After","type":"uint160"},{"internalType":"uint32","name":"initializedTicksCrossed","type":"uint32"},{"internalType":"uint256","name":"gasEstimate","type":"uint256"}],"stateMutability":"nonpayable","type":"function"}
]`

// NewMultiQuoter creates a new MultiQuoter instance.
func NewMultiQuoter(cfg *config.Config, log *zap.Logger) (Quoter, error) {
	ec, err := ethclient.Dial(cfg.Chain.RPCHTTP)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Ethereum client: %w", err)
	}

	q2abi, err := abi.JSON(strings.NewReader(quoterV2ABI))
	if err != nil {
		return nil, fmt.Errorf("failed to parse QuoterV2 ABI: %w", err)
	}

	multiABI, err := abi.JSON(strings.NewReader(multicallABI))
	if err != nil {
		return nil, fmt.Errorf("failed to parse Multicall3 ABI: %w", err)
	}

	ercABI, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		return nil, fmt.Errorf("failed to parse ERC20 ABI: %w", err)
	}

	return &MultiQuoter{
		cfg:      cfg,
		log:      log,
		ec:       ec,
		eabi:     ercABI,
		q2abi:    q2abi,
		multiabi: multiABI,
		decCache: make(map[common.Address]int),
	}, nil
}

func (q *MultiQuoter) QuoteDexOutUSD(ctx context.Context, tokenIn, tokenOut common.Address, qtyBase float64, ethUSD float64) (outUSD float64, gasUSD float64, feeTier uint32, err error) {
	if qtyBase <= 0 {
		return 0, 0, 0, fmt.Errorf("qtyBase must be > 0")
	}

	tiers := q.cfg.DEX.FeeTiers
	if len(tiers) == 0 {
		tiers = []uint32{500, 3000}
	}

	decIn, err := q.erc20Decimals(ctx, tokenIn)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get decimals for tokenIn %s: %w", tokenIn, err)
	}
	amountInWei := new(big.Int)
	new(big.Float).Mul(big.NewFloat(qtyBase), big.NewFloat(math.Pow10(decIn))).Int(amountInWei)

	quoterAddr := common.HexToAddress(q.cfg.DEX.QuoterV2)
	if quoterAddr == (common.Address{}) {
		return 0, 0, 0, fmt.Errorf("quoterV2 address not set in config")
	}

	type Call3 struct {
		Target       common.Address
		AllowFailure bool
		CallData     []byte
	}
	calls := make([]Call3, len(tiers))

	for i, fee := range tiers {
		params := struct {
			TokenIn           common.Address
			TokenOut          common.Address
			AmountIn          *big.Int
			Fee               *big.Int
			SqrtPriceLimitX96 *big.Int
		}{
			TokenIn:           tokenIn,
			TokenOut:          tokenOut,
			AmountIn:          amountInWei,
			Fee:               big.NewInt(int64(fee)),
			SqrtPriceLimitX96: big.NewInt(0),
		}
		callData, err := q.q2abi.Pack("quoteExactInputSingle", params)
		if err != nil {
			q.log.Warn("failed to pack quoteExactInputSingle", zap.Error(err), zap.Uint32("fee", fee))
			continue
		}
		calls[i] = Call3{
			Target:       quoterAddr,
			AllowFailure: true,
			CallData:     callData,
		}
	}

	multiCallData, err := q.multiabi.Pack("aggregate3", calls)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to pack aggregate3: %w", err)
	}

	multicallAddress := common.HexToAddress(Multicall3Address)
	res, err := q.ec.CallContract(ctx, ethereum.CallMsg{To: &multicallAddress, Data: multiCallData}, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("multicall failed: %w", err)
	}

	var results []struct {
		Success    bool
		ReturnData []byte
	}
	if err := q.multiabi.UnpackIntoInterface(&results, "aggregate3", res); err != nil {
		return 0, 0, 0, fmt.Errorf("failed to unpack aggregate3 results: %w", err)
	}

	decOut, err := q.erc20Decimals(ctx, tokenOut)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get decimals for tokenOut %s: %w", tokenOut, err)
	}

	var bestOut *big.Int
	var bestFee uint32

	for i, result := range results {
		if !result.Success {
			q.log.Debug("quote call failed in multicall", zap.Uint32("fee", tiers[i]))
			continue
		}

		unpacked, err := q.q2abi.Methods["quoteExactInputSingle"].Outputs.Unpack(result.ReturnData)
		if err != nil || len(unpacked) == 0 {
			q.log.Warn("failed to unpack quote result", zap.Error(err), zap.Uint32("fee", tiers[i]))
			continue
		}

		amountOut, ok := unpacked[0].(*big.Int)
		if !ok {
			q.log.Warn("unexpected type for amountOut", zap.Uint32("fee", tiers[i]))
			continue
		}

		if bestOut == nil || amountOut.Cmp(bestOut) > 0 {
			bestOut = amountOut
			bestFee = tiers[i]
		}
	}

	if bestOut == nil {
		return 0, 0, 0, fmt.Errorf("no working quoter found for any fee tier")
	}

	human := new(big.Float).Quo(new(big.Float).SetInt(bestOut), big.NewFloat(math.Pow10(decOut)))
	outUSD, _ = human.Float64()

	return outUSD, q.estimateGasUSD(ctx, ethUSD), bestFee, nil
}

func (q *MultiQuoter) QuoteDexInUSD(ctx context.Context, tokenIn, tokenOut common.Address, qtyBase float64, ethUSD float64) (inUSD float64, gasUSD float64, feeTier uint32, err error) {
	if qtyBase <= 0 {
		return 0, 0, 0, fmt.Errorf("qtyBase must be > 0")
	}
	tiers := q.cfg.DEX.FeeTiers
	if len(tiers) == 0 {
		tiers = []uint32{500, 3000}
	}

	decOut, err := q.erc20Decimals(ctx, tokenOut)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get decimals for tokenOut %s: %w", tokenOut, err)
	}
	amountOutWei := new(big.Int)
	new(big.Float).Mul(big.NewFloat(qtyBase), big.NewFloat(math.Pow10(decOut))).Int(amountOutWei)

	quoterAddr := common.HexToAddress(q.cfg.DEX.QuoterV2)
	if quoterAddr == (common.Address{}) {
		return 0, 0, 0, fmt.Errorf("quoterV2 address not set in config")
	}

	type Call3 struct {
		Target       common.Address
		AllowFailure bool
		CallData     []byte
	}
	calls := make([]Call3, len(tiers))

	for i, fee := range tiers {
		params := struct {
			TokenIn           common.Address
			TokenOut          common.Address
			Amount            *big.Int
			Fee               *big.Int
			SqrtPriceLimitX96 *big.Int
		}{
			TokenIn:           tokenIn,
			TokenOut:          tokenOut,
			Amount:            amountOutWei,
			Fee:               big.NewInt(int64(fee)),
			SqrtPriceLimitX96: big.NewInt(0),
		}
		callData, err := q.q2abi.Pack("quoteExactOutputSingle", params)
		if err != nil {
			q.log.Warn("failed to pack quoteExactOutputSingle", zap.Error(err), zap.Uint32("fee", fee))
			continue
		}
		calls[i] = Call3{
			Target:       quoterAddr,
			AllowFailure: true,
			CallData:     callData,
		}
	}

	multiCallData, err := q.multiabi.Pack("aggregate3", calls)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to pack aggregate3 for exact output: %w", err)
	}

	multicallAddress := common.HexToAddress(Multicall3Address)
	res, err := q.ec.CallContract(ctx, ethereum.CallMsg{To: &multicallAddress, Data: multiCallData}, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("multicall for exact output failed: %w", err)
	}

	var results []struct {
		Success    bool
		ReturnData []byte
	}
	if err := q.multiabi.UnpackIntoInterface(&results, "aggregate3", res); err != nil {
		return 0, 0, 0, fmt.Errorf("failed to unpack aggregate3 results for exact output: %w", err)
	}

	decIn, err := q.erc20Decimals(ctx, tokenIn)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get decimals for tokenIn %s: %w", tokenIn, err)
	}

	var bestIn *big.Int
	var bestFee uint32

	for i, result := range results {
		if !result.Success {
			q.log.Debug("quote exact output call failed in multicall", zap.Uint32("fee", tiers[i]))
			continue
		}

		unpacked, err := q.q2abi.Methods["quoteExactOutputSingle"].Outputs.Unpack(result.ReturnData)
		if err != nil || len(unpacked) == 0 {
			q.log.Warn("failed to unpack quote exact output result", zap.Error(err), zap.Uint32("fee", tiers[i]))
			continue
		}

		amountIn, ok := unpacked[0].(*big.Int)
		if !ok {
			q.log.Warn("unexpected type for amountIn", zap.Uint32("fee", tiers[i]))
			continue
		}

		if bestIn == nil || amountIn.Cmp(bestIn) < 0 {
			bestIn = amountIn
			bestFee = tiers[i]
		}
	}

	if bestIn == nil {
		return 0, 0, 0, fmt.Errorf("no working quoter found for any fee tier (exact output)")
	}

	human := new(big.Float).Quo(new(big.Float).SetInt(bestIn), big.NewFloat(math.Pow10(decIn)))
	inUSD, _ = human.Float64()

	return inUSD, q.estimateGasUSD(ctx, ethUSD), bestFee, nil
}

func (q *MultiQuoter) erc20Decimals(ctx context.Context, token common.Address) (int, error) {
	if (token == common.Address{}) {
		return 0, fmt.Errorf("decimals: zero token address")
	}

	q.decMu.RLock()
	if d, ok := q.decCache[token]; ok {
		q.decMu.RUnlock()
		return d, nil
	}
	q.decMu.RUnlock()

	input, err := q.eabi.Pack("decimals")
	if err != nil {
		return 0, fmt.Errorf("pack decimals: %w", err)
	}
	res, err := q.ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: input}, nil)
	if err != nil {
		return 0, fmt.Errorf("call decimals: %w", err)
	}
	outs, err := q.eabi.Methods["decimals"].Outputs.Unpack(res)
	if err != nil || len(outs) == 0 {
		return 0, fmt.Errorf("decode decimals: %w", err)
	}

	var dec int
	switch v := outs[0].(type) {
	case uint8:
		dec = int(v)
	case *big.Int:
		dec = int(v.Int64())
	default:
		return 0, fmt.Errorf("unexpected decimals type %T", v)
	}

	q.decMu.Lock()
	q.decCache[token] = dec
	q.decMu.Unlock()
	return dec, nil
}

func (q *MultiQuoter) estimateGasUSD(ctx context.Context, ethUSD float64) float64 {
	header, err := q.ec.HeaderByNumber(ctx, nil)
	if err != nil || header.BaseFee == nil {
		gp, e2 := q.ec.SuggestGasPrice(ctx)
		if e2 != nil {
			return 0
		}
		gasWei := new(big.Int).Mul(gp, new(big.Int).SetUint64(q.cfg.Chain.GasLimitSwap))
		return weiToUSD(gasWei, ethUSD)
	}
	tip, err := q.ec.SuggestGasTipCap(ctx)
	if err != nil {
		tip = big.NewInt(1e9) // 1 gwei fallback
	}
	eff := new(big.Int).Add(header.BaseFee, tip)
	gasWei := new(big.Int).Mul(eff, new(big.Int).SetUint64(q.cfg.Chain.GasLimitSwap))
	return weiToUSD(gasWei, ethUSD)
}
func weiToUSD(wei *big.Int, ethUSD float64) float64 {
	f := new(big.Float).SetInt(wei)
	f.Quo(f, big.NewFloat(1e18))
	f.Mul(f, big.NewFloat(ethUSD))
	out, _ := f.Float64()
	return out
}
