package univ3

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Uniswap v3 Factory — тот же адрес на Arbitrum
var UniswapV3Factory = common.HexToAddress("0x1F98431c8aD98523631AE4a59f267346ea31F984")

// минимальный ABI Factory: getPool(tokenA, tokenB, fee) -> address
const v3FactoryABI = `[
  {"inputs":[
    {"internalType":"address","name":"tokenA","type":"address"},
    {"internalType":"address","name":"tokenB","type":"address"},
    {"internalType":"uint24","name":"fee","type":"uint24"}],
   "name":"getPool",
   "outputs":[{"internalType":"address","name":"pool","type":"address"}],
   "stateMutability":"view","type":"function"}
]`

// CheckAvailableFeeTiers возвращает список существующих тиров и адреса пулов для BASE↔QUOTE.
func CheckAvailableFeeTiers(ctx context.Context, rpcURL string, base, quote common.Address, tiers []uint32) (present []uint32, pools map[uint32]common.Address, err error) {
	if (base == common.Address{}) || (quote == common.Address{}) {
		return nil, nil, fmt.Errorf("base/quote address is zero")
	}

	ec, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, nil, fmt.Errorf("dial rpc: %w", err)
	}
	defer ec.Close()

	fabi, err := abi.JSON(strings.NewReader(v3FactoryABI))
	if err != nil {
		return nil, nil, fmt.Errorf("parse factory abi: %w", err)
	}

	pools = make(map[uint32]common.Address, len(tiers))
	for _, fee := range tiers {
		// Uniswap требует: tokenA < tokenB (лексикографически)
		tokenA := base
		tokenB := quote
		if strings.ToLower(tokenB.Hex()) < strings.ToLower(tokenA.Hex()) {
			tokenA, tokenB = tokenB, tokenA
		}

		data, err := fabi.Pack("getPool", tokenA, tokenB, big.NewInt(int64(fee)))
		if err != nil {
			return nil, nil, fmt.Errorf("pack getPool: %w", err)
		}

		call := ethereum.CallMsg{
			To:   &UniswapV3Factory,
			Data: data,
		}
		// небольшой timeout на вызов
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		res, callErr := ec.CallContract(cctx, call, nil)
		cancel()
		if callErr != nil {
			return nil, nil, fmt.Errorf("call getPool(fee=%d): %w", fee, callErr)
		}

		out, err := fabi.Unpack("getPool", res)
		if err != nil || len(out) != 1 {
			return nil, nil, fmt.Errorf("unpack getPool(fee=%d): %w", fee, err)
		}
		addr := out[0].(common.Address)
		if addr != (common.Address{}) {
			present = append(present, fee)
			pools[fee] = addr
		}
	}
	return present, pools, nil
}

// PrettyUSD просто хелпер если захочешь печатать стоимости (не обязателен)
func PrettyUSD(x float64) string {
	return fmt.Sprintf("$%.6f", x)
}

// BigIntFromFloat — хелпер, если будешь пробовать квоты вручную
func BigIntFromFloat(x float64, decimals int) *big.Int {
	mul := new(big.Float).SetFloat64(x)
	for i := 0; i < decimals; i++ {
		mul.Mul(mul, big.NewFloat(10))
	}
	out, _ := mul.Int(nil)
	return out
}
