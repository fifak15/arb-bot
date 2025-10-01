package univ3

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/multicall"
	"go.uber.org/zap"
)

// MultiQuoter is a batch-quoter for Uniswap V3 using a multicall contract.
type MultiQuoter struct {
	log    *zap.Logger
	cfg    *config.Config
	ec     *ethclient.Client
	mc     multicall.IClient
	q2abi  abi.ABI
	quoter common.Address

	// Cache for token decimals to avoid repeated lookups
	decimalsCache sync.Map
}

type QuoteType int

const (
	QuoteTypeExactInput QuoteType = iota
	QuoteTypeExactOutput
)

type MultiQuoteRequest struct {
	PairSymbol string
	TokenIn    common.Address
	TokenOut   common.Address
	Amount     *big.Int // AmountIn for ExactInput, AmountOut for ExactOutput
	FeeTiers   []uint32
	Type       QuoteType
}

type MultiQuoteResult struct {
	Amount    *big.Int // AmountOut for ExactInput, AmountIn for ExactOutput
	AmountUSD float64  // Human-readable amount
	FeeTier   uint32
	Error     error
}

func NewMultiQuoter(cfg *config.Config, log *zap.Logger) (*MultiQuoter, error) {
	ec, err := ethclient.Dial(cfg.Chain.RPCHTTP)
	if err != nil {
		return nil, fmt.Errorf("dial rpc: %w", err)
	}

	mcAddr := common.HexToAddress(cfg.DEX.Multicall)
	if mcAddr == (common.Address{}) {
		return nil, fmt.Errorf("multicall address is not configured")
	}
	mc, err := multicall.New(ec, mcAddr)
	if err != nil {
		return nil, fmt.Errorf("new multicall client: %w", err)
	}

	q2abi, err := abi.JSON(strings.NewReader(quoterV2ABI))
	if err != nil {
		return nil, fmt.Errorf("parse quoter v2 abi: %w", err)
	}

	quoterAddr := common.HexToAddress(cfg.DEX.QuoterV2)
	if quoterAddr == (common.Address{}) {
		return nil, fmt.Errorf("quoter v2 address is not configured")
	}

	return &MultiQuoter{
		log:    log,
		cfg:    cfg,
		ec:     ec,
		mc:     mc,
		q2abi:  q2abi,
		quoter: quoterAddr,
	}, nil
}

func (mq *MultiQuoter) QuoteAll(ctx context.Context, reqs []MultiQuoteRequest) (map[string]MultiQuoteResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}

	var calls []multicall.Call
	type callInfo struct {
		reqIdx  int
		feeTier uint32
		reqType QuoteType
	}
	var callMetas []callInfo

	for i, req := range reqs {
		for _, fee := range req.FeeTiers {
			var methodName string
			var params interface{}

			if req.Type == QuoteTypeExactInput {
				methodName = "quoteExactInputSingle"
				params = mq.buildExactInputParams(req.TokenIn, req.TokenOut, req.Amount, fee)
			} else {
				methodName = "quoteExactOutputSingle"
				params = mq.buildExactOutputParams(req.TokenIn, req.TokenOut, req.Amount, fee)
			}

			callData, err := mq.q2abi.Pack(methodName, params)
			if err != nil {
				mq.log.Warn("failed to pack quote data", zap.Error(err), zap.String("pair", req.PairSymbol), zap.String("method", methodName))
				continue
			}
			calls = append(calls, multicall.Call{
				Target:   mq.quoter,
				CallData: callData,
			})
			callMetas = append(callMetas, callInfo{reqIdx: i, feeTier: fee, reqType: req.Type})
		}
	}

	if len(calls) == 0 {
		return nil, fmt.Errorf("no valid calls could be constructed")
	}

	results, err := mq.mc.Aggregate(ctx, calls)
	if err != nil {
		return nil, fmt.Errorf("multicall aggregate failed: %w", err)
	}

	bestQuotes := make(map[string]MultiQuoteResult)

	for i, res := range results {
		if !res.Success {
			continue
		}

		meta := callMetas[i]
		originalReq := reqs[meta.reqIdx]

		var methodName string
		var quoteToken common.Address
		if meta.reqType == QuoteTypeExactInput {
			methodName = "quoteExactInputSingle"
			quoteToken = originalReq.TokenOut
		} else {
			methodName = "quoteExactOutputSingle"
			quoteToken = originalReq.TokenIn
		}

		unpacked, err := mq.q2abi.Methods[methodName].Outputs.Unpack(res.Data)
		if err != nil || len(unpacked) == 0 {
			continue
		}

		amount, ok := unpacked[0].(*big.Int)
		if !ok {
			continue
		}

		decimals, err := mq.getDecimals(ctx, quoteToken)
		if err != nil {
			mq.log.Warn("failed to get decimals", zap.Error(err), zap.String("token", quoteToken.Hex()))
			continue
		}
		amountUSD := ToFloat(amount, decimals)

		currentBest, found := bestQuotes[originalReq.PairSymbol]
		isBetter := false
		if !found {
			isBetter = true
		} else {
			if originalReq.Type == QuoteTypeExactInput && amount.Cmp(currentBest.Amount) > 0 {
				isBetter = true
			} else if originalReq.Type == QuoteTypeExactOutput && amount.Cmp(currentBest.Amount) < 0 {
				isBetter = true
			}
		}

		if isBetter {
			bestQuotes[originalReq.PairSymbol] = MultiQuoteResult{
				Amount:    amount,
				AmountUSD: amountUSD,
				FeeTier:   meta.feeTier,
			}
		}
	}

	for _, req := range reqs {
		if _, found := bestQuotes[req.PairSymbol]; !found {
			bestQuotes[req.PairSymbol] = MultiQuoteResult{
				Error: fmt.Errorf("no successful quote for any fee tier"),
			}
		}
	}

	return bestQuotes, nil
}

func (mq *MultiQuoter) EstimateGasUSD(ctx context.Context, ethPrice float64) (float64, error) {
	header, err := mq.ec.HeaderByNumber(ctx, nil)
	if err != nil || header.BaseFee == nil {
		gp, err := mq.ec.SuggestGasPrice(ctx)
		if err != nil {
			return 0, fmt.Errorf("suggest gas price: %w", err)
		}
		gasWei := new(big.Int).Mul(gp, new(big.Int).SetUint64(mq.cfg.Chain.GasLimitSwap))
		return weiToUSD(gasWei, ethPrice), nil
	}
	tip, err := mq.ec.SuggestGasTipCap(ctx)
	if err != nil {
		tip = big.NewInt(1e9) // fallback to 1 gwei
	}
	eff := new(big.Int).Add(header.BaseFee, tip)
	gasWei := new(big.Int).Mul(eff, new(big.Int).SetUint64(mq.cfg.Chain.GasLimitSwap))
	return weiToUSD(gasWei, ethPrice), nil
}

func (mq *MultiQuoter) getDecimals(ctx context.Context, token common.Address) (int, error) {
	if dec, ok := mq.decimalsCache.Load(token); ok {
		return dec.(int), nil
	}
	dec, err := GetERC20Decimals(ctx, mq.ec, token)
	if err != nil {
		return 0, err
	}
	mq.decimalsCache.Store(token, dec)
	return dec, nil
}

func (mq *MultiQuoter) buildExactInputParams(tokenIn, tokenOut common.Address, amountIn *big.Int, fee uint32) interface{} {
	return struct {
		TokenIn           common.Address
		TokenOut          common.Address
		AmountIn          *big.Int
		Fee               *big.Int
		SqrtPriceLimitX96 *big.Int
	}{
		TokenIn:           tokenIn,
		TokenOut:          tokenOut,
		AmountIn:          amountIn,
		Fee:               big.NewInt(int64(fee)),
		SqrtPriceLimitX96: big.NewInt(0),
	}
}

func (mq *MultiQuoter) buildExactOutputParams(tokenIn, tokenOut common.Address, amountOut *big.Int, fee uint32) interface{} {
	return struct {
		TokenIn           common.Address
		TokenOut          common.Address
		Amount            *big.Int
		Fee               *big.Int
		SqrtPriceLimitX96 *big.Int
	}{
		TokenIn:           tokenIn,
		TokenOut:          tokenOut,
		Amount:            amountOut,
		Fee:               big.NewInt(int64(fee)),
		SqrtPriceLimitX96: big.NewInt(0),
	}
}