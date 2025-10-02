package execution

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/types"
	"go.uber.org/zap"
)

const erc20ABI = `[{"inputs":[],"name":"decimals","outputs":[{"internalType":"uint8","name":"","type":"uint8"}],"stateMutability":"view","type":"function"}]`

type cexIface interface {
	PlaceIOC(ctx context.Context, symbol string, side string, qty float64, price float64) (orderID string, filledQty, avgPrice float64, err error)
}

type routerIface interface {
	SwapExactInput(ctx context.Context, tokenIn, tokenOut common.Address, amountIn *big.Int, minOut *big.Int, feeTier uint32) (txHash string, err error)
	SwapExactOutput(ctx context.Context, tokenIn, tokenOut common.Address, amountOut *big.Int, maxIn *big.Int, feeTier uint32) (txHash string, err error)
}

type Risk interface {
	AllowTrade(netUSD, roi float64) bool
}

type Executor struct {
	cfg          *config.Config
	cex          cexIface
	router       routerIface
	risk         Risk
	log          *zap.Logger
	ec           *ethclient.Client
	usdxDecimals int

	baseAddr     common.Address
	baseDecimals int
}

func NewExecutor(cfg *config.Config, cex cexIface, router routerIface, risk Risk, log *zap.Logger, baseAddr common.Address) (*Executor, error) {
	ec, err := ethclient.Dial(cfg.Chain.RPCHTTP)
	if err != nil {
		return nil, fmt.Errorf("dial rpc for executor: %w", err)
	}

	e := &Executor{
		cfg:      cfg,
		cex:      cex,
		router:   router,
		risk:     risk,
		log:      log,
		ec:       ec,
		baseAddr: baseAddr,
	}

	usdxDec, err := e.fetchDecimals(context.Background(), common.HexToAddress(cfg.DEX.USDT))
	if err != nil {
		return nil, fmt.Errorf("fetch stablecoin decimals: %w", err)
	}
	e.usdxDecimals = usdxDec

	baseDec, err := e.fetchDecimals(context.Background(), baseAddr)
	if err != nil {
		return nil, fmt.Errorf("fetch base token decimals: %w", err)
	}
	e.baseDecimals = baseDec

	e.log.Info("Executor initialized",
		zap.Int("stablecoin_decimals", usdxDec),
		zap.Int("base_token_decimals", baseDec),
		zap.String("base_token_addr", baseAddr.Hex()),
	)

	return e, nil
}

func (e *Executor) Run(ctx context.Context, in <-chan types.Opportunity) {
	for {
		select {
		case <-ctx.Done():
			return
		case opp := <-in:
			if !e.risk.AllowTrade(opp.NetUSD, opp.ROI) {
				e.log.Debug("Trade rejected by risk management", zap.Float64("netUSD", opp.NetUSD), zap.Float64("roi", opp.ROI))
				continue
			}
			if e.router == nil {
				e.log.Error("Router not initialized, cannot execute swap")
				continue
			}

			switch opp.Direction {
			case types.CEXBuyDEXSell:
				e.handleCexBuyDexSell(ctx, opp)
			case types.DEXBuyCEXSell:
				e.handleDexBuyCexSell(ctx, opp)
			default:
				e.log.Error("Unknown opportunity direction", zap.String("direction", string(opp.Direction)))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (e *Executor) handleCexBuyDexSell(ctx context.Context, opp types.Opportunity) {
	log := e.log.With(zap.String("direction", "CEX_BUY_DEX_SELL"))
	log.Info("Executing opportunity")

	orderID, filled, avgPx, err := e.cex.PlaceIOC(ctx, e.cfg.Pair, "BUY", opp.QtyBase, opp.BuyPxCEX)
	if err != nil || filled <= 0 {
		log.Warn("IOC on CEX failed or not filled", zap.Error(err), zap.Float64("filled", filled))
		return
	}
	log.Info("CEX buy order filled", zap.String("orderID", orderID), zap.Float64("filled_qty", filled), zap.Float64("avg_px", avgPx))

	scale := filled / opp.QtyBase
	expectedOutUSD := opp.DexOutUSD * scale
	slippage := 1.0 - float64(e.cfg.Risk.MaxSlippageBps)/10000.0

	minOut := new(big.Float).Mul(big.NewFloat(expectedOutUSD*slippage), big.NewFloat(math.Pow10(e.usdxDecimals)))
	minOutInt, _ := minOut.Int(nil)

	scaleBase := new(big.Float).SetFloat64(math.Pow10(e.baseDecimals))
	inBase := new(big.Int)
	new(big.Float).Mul(big.NewFloat(filled), scaleBase).Int(inBase)

	baseAddr := e.baseAddr
	usdxAddr := common.HexToAddress(e.cfg.DEX.USDT)

	tx, err := e.router.SwapExactInput(ctx, baseAddr, usdxAddr, inBase, minOutInt, opp.DexFeeTier)
	if err != nil {
		log.Error("DEX swap failed, attempting to reverse CEX trade", zap.Error(err))
		_, _, _, revErr := e.cex.PlaceIOC(ctx, e.cfg.Pair, "SELL", filled, avgPx*(1.0-0.01)) // -1% для вероятного мгновенного исполнения
		if revErr != nil {
			log.Error("FATAL: CEX reversal trade failed, manual intervention required", zap.Error(revErr), zap.Float64("stuck_qty", filled))
		} else {
			log.Warn("CEX reversal trade executed successfully")
		}
		return
	}

	log.Info("TRADE COMPLETE: CEX_BUY_DEX_SELL",
		zap.String("cex_order_id", orderID),
		zap.String("dex_tx_hash", tx),
		zap.Float64("net_profit_usd", opp.NetUSD),
		zap.Float64("roi_bps", opp.ROI*10000),
	)
}

func (e *Executor) handleDexBuyCexSell(ctx context.Context, opp types.Opportunity) {
	log := e.log.With(zap.String("direction", "DEX_BUY_CEX_SELL"))
	log.Info("Executing opportunity")

	scaleBase := new(big.Float).SetFloat64(math.Pow10(e.baseDecimals))
	amountOutBase := new(big.Int)
	new(big.Float).Mul(big.NewFloat(opp.QtyBase), scaleBase).Int(amountOutBase)

	slippage := 1.0 + float64(e.cfg.Risk.MaxSlippageBps)/10000.0
	maxIn := new(big.Float).Mul(big.NewFloat(opp.DexInUSD*slippage), big.NewFloat(math.Pow10(e.usdxDecimals)))
	maxInInt, _ := maxIn.Int(nil)

	baseAddr := e.baseAddr
	usdxAddr := common.HexToAddress(e.cfg.DEX.USDT)

	tx, err := e.router.SwapExactOutput(ctx, usdxAddr, baseAddr, amountOutBase, maxInInt, opp.DexFeeTier)
	if err != nil {
		log.Error("DEX swap failed", zap.Error(err))
		return
	}
	log.Info("DEX buy swap executed", zap.String("tx_hash", tx))

	// 2) Продаём на CEX купленный объём базового токена
	orderID, filled, avgPx, err := e.cex.PlaceIOC(ctx, e.cfg.Pair, "SELL", opp.QtyBase, opp.SellPxCEX)
	if err != nil || filled < opp.QtyBase*e.cfg.Risk.MinFillRatio {
		log.Error("IOC on CEX failed or not filled sufficiently, attempting to reverse DEX trade", zap.Error(err), zap.Float64("filled_qty", filled))
		// REVERSAL: продаём купленный базовый токен обратно на DEX за стейбл
		_, revErr := e.router.SwapExactInput(ctx, baseAddr, usdxAddr, amountOutBase, big.NewInt(0), opp.DexFeeTier)
		if revErr != nil {
			log.Error("FATAL: DEX reversal trade failed, manual intervention required", zap.Error(revErr), zap.Float64("stuck_qty", opp.QtyBase))
		} else {
			log.Warn("DEX reversal trade executed successfully")
		}
		return
	}

	log.Info("TRADE COMPLETE: DEX_BUY_CEX_SELL",
		zap.String("dex_tx_hash", tx),
		zap.String("cex_order_id", orderID),
		zap.Float64("filled_qty", filled),
		zap.Float64("avg_px", avgPx),
		zap.Float64("net_profit_usd", opp.NetUSD),
		zap.Float64("roi_bps", opp.ROI*10000),
	)
}

func (e *Executor) fetchDecimals(ctx context.Context, token common.Address) (int, error) {
	eabi, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		return 0, err
	}
	input, err := eabi.Pack("decimals")
	if err != nil {
		return 0, fmt.Errorf("pack decimals: %w", err)
	}
	res, err := e.ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: input}, nil)
	if err != nil {
		return 0, fmt.Errorf("call decimals: %w", err)
	}
	outs, err := eabi.Methods["decimals"].Outputs.Unpack(res)
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
