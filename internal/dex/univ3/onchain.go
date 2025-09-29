package univ3

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum" // CallMsg
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"go.uber.org/zap"

	"github.com/you/arb-bot/internal/config"
)

type Quoter interface {
	// outUSD — ожидаемая выручка в USDT/USDC (1:1 к USD), gasUSD — оценка газа в USD.
	QuoteDexOutUSD(ctx context.Context, qtyBase float64, ethUSD float64) (outUSD float64, gasUSD float64, err error)
}

type Router interface {
	// amountInWei — количество WETH в wei, minOut — минимальный out в единицах стейблкоина (6 знаков).
	SwapExactInput(ctx context.Context, amountInWei *big.Int, minOut uint64) (txHash string, err error)
}

type slot0Quoter struct {
	cfg   *config.Config
	log   *zap.Logger
	ec    *ethclient.Client
	eabi  abi.ABI // ERC-20 (decimals)
	fabi  abi.ABI // Factory (getPool)
	pabi  abi.ABI // Pool (slot0, token0/1, liquidity)
	q1abi abi.ABI // QuoterV1 (quoteExactInputSingle)
	pools map[uint32]common.Address
}

const v3FactoryAddr = "0x1F98431c8aD98523631AE4a59f267346ea31F984"

func (q *slot0Quoter) erc20Decimals(ctx context.Context, token common.Address) (int, error) {
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

	switch v := outs[0].(type) {
	case uint8:
		return int(v), nil
	case *big.Int:
		return int(v.Int64()), nil
	default:
		return 0, fmt.Errorf("unexpected decimals type %T", v)
	}
}

const factoryABI = `[
  {"inputs":[
     {"internalType":"address","name":"tokenA","type":"address"},
     {"internalType":"address","name":"tokenB","type":"address"},
     {"internalType":"uint24","name":"fee","type":"uint24"}],
   "name":"getPool","outputs":[{"internalType":"address","name":"pool","type":"address"}],
   "stateMutability":"view","type":"function"}
]`

const erc20ABI = `[
  {"inputs":[],"name":"decimals","outputs":[{"internalType":"uint8","name":"","type":"uint8"}],"stateMutability":"view","type":"function"}
]`

// Минимальный ABI пула для чтения slot0 и token0/token1 + liquidity
const poolABI = `[
  {"inputs":[],"name":"slot0","outputs":[
     {"internalType":"uint160","name":"sqrtPriceX96","type":"uint160"},
     {"internalType":"int24","name":"tick","type":"int24"},
     {"internalType":"uint16","name":"observationIndex","type":"uint16"},
     {"internalType":"uint16","name":"observationCardinality","type":"uint16"},
     {"internalType":"uint16","name":"observationCardinalityNext","type":"uint16"},
     {"internalType":"uint8","name":"feeProtocol","type":"uint8"},
     {"internalType":"bool","name":"unlocked","type":"bool"}],
   "stateMutability":"view","type":"function"},
  {"inputs":[],"name":"token0","outputs":[{"internalType":"address","name":"","type":"address"}],"stateMutability":"view","type":"function"},
  {"inputs":[],"name":"token1","outputs":[{"internalType":"address","name":"","type":"address"}],"stateMutability":"view","type":"function"},
  {"inputs":[],"name":"liquidity","outputs":[{"internalType":"uint128","name":"","type":"uint128"}],"stateMutability":"view","type":"function"}
]`

// Quoter V1
const quoterV1ABI = `[
  {"inputs":[
     {"internalType":"address","name":"tokenIn","type":"address"},
     {"internalType":"address","name":"tokenOut","type":"address"},
     {"internalType":"uint24","name":"fee","type":"uint24"},
     {"internalType":"uint256","name":"amountIn","type":"uint256"},
     {"internalType":"uint160","name":"sqrtPriceLimitX96","type":"uint160"}],
   "name":"quoteExactInputSingle",
   "outputs":[{"internalType":"uint256","name":"amountOut","type":"uint256"}],
   "stateMutability":"nonpayable","type":"function"}]`

// Конструктор
func NewSlot0Quoter(cfg *config.Config, log *zap.Logger) (Quoter, error) {
	ec, err := ethclient.Dial(cfg.Chain.RPCHTTP)
	if err != nil {
		return nil, err
	}
	fabi, _ := abi.JSON(strings.NewReader(factoryABI))
	pabi, _ := abi.JSON(strings.NewReader(poolABI))
	ercABI, _ := abi.JSON(strings.NewReader(erc20ABI))
	q1abi, _ := abi.JSON(strings.NewReader(quoterV1ABI))
	return &slot0Quoter{
		cfg:   cfg,
		log:   log,
		ec:    ec,
		fabi:  fabi,
		pabi:  pabi,
		eabi:  ercABI,
		q1abi: q1abi,
		pools: make(map[uint32]common.Address),
	}, nil
}

func (q *slot0Quoter) quoteExactInputSingleV1(ctx context.Context, fee uint32, amountInWei *big.Int) (*big.Int, error) {
	quoter := common.HexToAddress(q.cfg.DEX.QuoterV1)
	if quoter == (common.Address{}) {
		return nil, fmt.Errorf("quoterV1 address not set")
	}
	weth := common.HexToAddress(q.cfg.DEX.WETH)
	usdx := common.HexToAddress(q.cfg.DEX.USDT)

	input, err := q.q1abi.Pack("quoteExactInputSingle",
		weth, usdx, big.NewInt(int64(fee)), amountInWei, big.NewInt(0),
	)
	if err != nil {
		return nil, fmt.Errorf("pack quoterV1: %w", err)
	}
	from := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	msg := ethereum.CallMsg{From: from, To: &quoter, Gas: 2_000_000, Data: input}

	// Сначала 'pending', затем 'latest' — повышает совместимость с разными RPC
	res, err := q.ec.PendingCallContract(ctx, msg)
	if err != nil {
		res, err = q.ec.CallContract(ctx, msg, nil)
		if err != nil {
			return nil, fmt.Errorf("call quoterV1: %w", err)
		}
	}
	outs, err := q.q1abi.Methods["quoteExactInputSingle"].Outputs.Unpack(res)
	if err != nil || len(outs) == 0 {
		return nil, fmt.Errorf("decode quoterV1: %w", err)
	}
	return outs[0].(*big.Int), nil
}

// --- Оценка газа в USD (EIP-1559 aware) ---

func (q *slot0Quoter) estimateGasUSD(ctx context.Context, ethUSD float64) float64 {
	header, err := q.ec.HeaderByNumber(ctx, nil)
	if err != nil || header.BaseFee == nil {
		// Фоллбек на legacy-режим
		gp, e2 := q.ec.SuggestGasPrice(ctx)
		if e2 != nil {
			return 0
		}
		gasWei := new(big.Int).Mul(gp, new(big.Int).SetUint64(q.cfg.Chain.GasLimitSwap))
		return weiToUSD(gasWei, ethUSD)
	}
	tip, err := q.ec.SuggestGasTipCap(ctx)
	if err != nil {
		tip = big.NewInt(1e9) // 1 gwei по умолчанию
	}
	eff := new(big.Int).Add(header.BaseFee, tip)
	gasWei := new(big.Int).Mul(eff, new(big.Int).SetUint64(q.cfg.Chain.GasLimitSwap))
	return weiToUSD(gasWei, ethUSD)
}

// --- Публичная котировка ---

func (q *slot0Quoter) QuoteDexOutUSD(ctx context.Context, qtyBase float64, ethUSD float64) (outUSD float64, gasUSD float64, err error) {
	if qtyBase <= 0 {
		return 0, 0, fmt.Errorf("qtyBase must be > 0")
	}
	tiers := q.cfg.DEX.FeeTiers
	if len(tiers) == 0 {
		if q.cfg.DEX.FeeTier != 0 {
			tiers = []uint32{q.cfg.DEX.FeeTier}
		} else {
			tiers = []uint32{500, 3000}
		}
	}

	// qtyBase ETH -> wei
	amountInWei := new(big.Int)
	new(big.Float).Mul(big.NewFloat(qtyBase), big.NewFloat(1e18)).Int(amountInWei)

	// decimals стейбла (USDX)
	usdx := common.HexToAddress(q.cfg.DEX.USDT)
	decUSDX, derr := q.erc20Decimals(ctx, usdx)
	if derr != nil {
		return 0, 0, derr
	}

	// 1) Пробуем Quoter V1 по всем fee и берём лучший amountOut
	var bestOut *big.Int
	for _, fee := range tiers {
		amt, e := q.quoteExactInputSingleV1(ctx, fee, amountInWei)
		if e != nil {
			q.log.Warn("QuoterV1: не удалось получить котировку", zap.Uint32("fee", fee), zap.Error(e))
			continue
		}
		if bestOut == nil || amt.Cmp(bestOut) > 0 {
			bestOut = amt
		}
	}

	if bestOut != nil {
		human := new(big.Float).Quo(new(big.Float).SetInt(bestOut), big.NewFloat(math.Pow10(decUSDX)))
		outUSD, _ = human.Float64()
		q.log.Info("DEX-котировка (Quoter V1) выбрана",
			zap.String("amount_out_raw", bestOut.String()),
			zap.Int("usdx_decimals", decUSDX),
			zap.Float64("amount_out_human", outUSD),
		)
		return outUSD, q.estimateGasUSD(ctx, ethUSD), nil
	}

	// 2) Фоллбек: расчёт по slot0
	var lastErr error
	for _, tier := range tiers {
		pool, e := q.getPool(ctx, tier)
		if e != nil || (pool == (common.Address{})) {
			lastErr = fmt.Errorf("getPool fee %d: %w", tier, e)
			continue
		}
		usdxPerWeth, e := q.readSpotUSDXPerWETH(ctx, pool)
		if e != nil {
			lastErr = fmt.Errorf("slot0 fee %d: %w", tier, e)
			continue
		}
		outUSD = qtyBase * usdxPerWeth
		q.log.Info("DEX-котировка (slot0) выбрана", zap.Uint32("fee", tier), zap.Float64("out_usd", outUSD))
		return outUSD, q.estimateGasUSD(ctx, ethUSD), nil
	}

	if lastErr != nil {
		return 0, 0, lastErr
	}
	return 0, 0, fmt.Errorf("no working fee tier")
}

// --- Factory getPool (+ проверка ликвидности) ---

func (q *slot0Quoter) getPool(ctx context.Context, fee uint32) (common.Address, error) {
	if addr, ok := q.pools[fee]; ok && addr != (common.Address{}) {
		q.log.Debug("getPool: найдено в кэше", zap.Uint32("fee", fee), zap.String("pool", addr.Hex()))
		return addr, nil
	}
	weth := common.HexToAddress(q.cfg.DEX.WETH)
	usdx := common.HexToAddress(q.cfg.DEX.USDT)

	// Важно: *big.Int для uint24 и обработка ошибки
	input, err := q.fabi.Pack("getPool", weth, usdx, big.NewInt(int64(fee)))
	if err != nil {
		q.log.Warn("getPool: ошибка упаковки параметров", zap.Uint32("fee", fee), zap.Error(err))
		return common.Address{}, fmt.Errorf("pack getPool: %w", err)
	}

	faddr := common.HexToAddress(v3FactoryAddr)
	q.log.Debug("getPool: вызов метода",
		zap.String("factory", faddr.Hex()),
		zap.String("weth", weth.Hex()), zap.String("usdx", usdx.Hex()),
		zap.Uint32("fee", fee),
	)

	res, err := q.ec.CallContract(ctx, ethereum.CallMsg{To: &faddr, Data: input}, nil)
	if err != nil {
		q.log.Warn("getPool: вызов контракта не удался", zap.Uint32("fee", fee), zap.Error(err))
		return common.Address{}, fmt.Errorf("call getPool: %w", err)
	}
	outs, err := q.fabi.Methods["getPool"].Outputs.Unpack(res)
	if err != nil || len(outs) == 0 {
		if err == nil {
			err = fmt.Errorf("empty getPool output")
		}
		q.log.Warn("getPool: не удалось декодировать ответ", zap.Uint32("fee", fee), zap.Error(err))
		return common.Address{}, fmt.Errorf("decode getPool: %w", err)
	}
	pool := outs[0].(common.Address)
	if pool == (common.Address{}) {
		q.log.Warn("getPool: пул не найден (пустой адрес)", zap.Uint32("fee", fee))
		return common.Address{}, fmt.Errorf("no pool for fee %d", fee)
	}

	// Проверяем, что в пуле есть ликвидность
	liqCall, _ := q.pabi.Pack("liquidity")
	liqRaw, err := q.ec.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: liqCall}, nil)
	if err == nil {
		liqOut, err := q.pabi.Methods["liquidity"].Outputs.Unpack(liqRaw)
		if err == nil && len(liqOut) > 0 {
			if liq, ok := liqOut[0].(*big.Int); ok && liq.Sign() == 0 {
				q.log.Warn("getPool: у пула нулевая ликвидность", zap.Uint32("fee", fee), zap.String("pool", pool.Hex()))
				return common.Address{}, fmt.Errorf("pool %s has zero liquidity", pool.Hex())
			}
		}
	}

	q.pools[fee] = pool
	q.log.Info("getPool: пул найден", zap.Uint32("fee", fee), zap.String("pool", pool.Hex()))
	return pool, nil
}

func (q *slot0Quoter) readSpotUSDXPerWETH(ctx context.Context, pool common.Address) (float64, error) {
	q.log.Debug("slot0: начинаю чтение цены", zap.String("pool", pool.Hex()))

	call := func(name string) ([]byte, error) {
		input, err := q.pabi.Pack(name)
		if err != nil {
			q.log.Warn("slot0: не удалось упаковать вызов", zap.String("method", name), zap.Error(err))
			return nil, err
		}
		return q.ec.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: input}, nil)
	}

	// token0
	res0, err := call("token0")
	if err != nil {
		return 0, err
	}
	outs0, err := q.pabi.Methods["token0"].Outputs.Unpack(res0)
	if err != nil || len(outs0) == 0 {
		return 0, fmt.Errorf("decode token0: %w", err)
	}
	token0 := outs0[0].(common.Address)

	// token1
	res1, err := call("token1")
	if err != nil {
		return 0, err
	}
	outs1, err := q.pabi.Methods["token1"].Outputs.Unpack(res1)
	if err != nil || len(outs1) == 0 {
		return 0, fmt.Errorf("decode token1: %w", err)
	}
	token1 := outs1[0].(common.Address)

	q.log.Debug("slot0: адреса токенов пула", zap.String("token0", token0.Hex()), zap.String("token1", token1.Hex()))

	// slot0
	inputSlot0, err := q.pabi.Pack("slot0")
	if err != nil {
		return 0, fmt.Errorf("pack slot0: %w", err)
	}
	resS, err := q.ec.CallContract(ctx, ethereum.CallMsg{To: &pool, Data: inputSlot0}, nil)
	if err != nil {
		return 0, err
	}
	outsS, err := q.pabi.Methods["slot0"].Outputs.Unpack(resS)
	if err != nil || len(outsS) == 0 {
		return 0, fmt.Errorf("decode slot0: %w", err)
	}

	sqrtPriceX96 := outsS[0].(*big.Int)

	tickBI, ok := outsS[1].(*big.Int)
	if !ok {
		return 0, fmt.Errorf("unexpected tick type %T", outsS[1])
	}
	ti := tickBI.Int64()
	if ti > math.MaxInt32 || ti < math.MinInt32 {
		return 0, fmt.Errorf("tick out of int32 range: %d", ti)
	}
	tick := int32(ti)

	q.log.Debug("slot0: чтение успешно",
		zap.String("sqrtPriceX96", sqrtPriceX96.String()),
		zap.Int32("tick", tick),
	)

	weth := common.HexToAddress(q.cfg.DEX.WETH)
	usdx := common.HexToAddress(q.cfg.DEX.USDT)

	priceToken1PerToken0, err := uniswapPriceFromSqrt(sqrtPriceX96)
	if err != nil {
		return 0, err
	}

	// читаем decimals(token0), decimals(token1)
	dec0, err := q.erc20Decimals(ctx, token0)
	if err != nil {
		return 0, err
	}
	dec1, err := q.erc20Decimals(ctx, token1)
	if err != nil {
		return 0, err
	}

	// масштабный коэффициент 10^(dec0 - dec1)
	scale := math.Pow10(dec0 - dec1)
	humanP1perP0 := priceToken1PerToken0 * scale
	q.log.Debug("slot0: расчёт цены для человека",
		zap.Float64("raw_p1_per_p0", priceToken1PerToken0),
		zap.Int("dec0", dec0),
		zap.Int("dec1", dec1),
		zap.Float64("scale", scale),
		zap.Float64("human_p1_per_p0", humanP1perP0),
	)
	switch {
	case token0 == weth && token1 == usdx:
		// хотим USDX за 1 WETH
		return humanP1perP0, nil
	case token0 == usdx && token1 == weth:
		if humanP1perP0 == 0 {
			return 0, fmt.Errorf("zero price")
		}
		return 1.0 / humanP1perP0, nil
	default:
		return 0, fmt.Errorf("pool tokens mismatch (token0=%s token1=%s)", token0.Hex(), token1.Hex())
	}
}

func uniswapPriceFromSqrt(sqrtPriceX96 *big.Int) (float64, error) {
	if sqrtPriceX96.Sign() <= 0 {
		return 0, fmt.Errorf("bad sqrtPriceX96")
	}
	f := new(big.Float).SetPrec(256).SetInt(sqrtPriceX96)
	f.Mul(f, f)
	den := new(big.Float).SetPrec(256).SetFloat64(math.Exp2(192))
	f.Quo(f, den)
	out, _ := f.Float64()
	return out, nil
}

func weiToUSD(wei *big.Int, ethUSD float64) float64 {
	f := new(big.Float).SetInt(wei)
	f.Quo(f, big.NewFloat(1e18))
	f.Mul(f, big.NewFloat(ethUSD))
	out, _ := f.Float64()
	return out
}
