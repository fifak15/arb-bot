package univ3

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const erc20ABIForUtils = `[
  {"inputs":[],"name":"decimals","outputs":[{"internalType":"uint8","name":"","type":"uint8"}],"stateMutability":"view","type":"function"}
]`

func GetERC20Decimals(ctx context.Context, ec *ethclient.Client, token common.Address) (int, error) {
	parsedABI, err := abi.JSON(strings.NewReader(erc20ABIForUtils))
	if err != nil {
		return 0, fmt.Errorf("bad abi: %w", err)
	}

	input, err := parsedABI.Pack("decimals")
	if err != nil {
		return 0, fmt.Errorf("pack decimals: %w", err)
	}
	res, err := ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: input}, nil)
	if err != nil {
		return 0, fmt.Errorf("call decimals: %w", err)
	}
	outs, err := parsedABI.Methods["decimals"].Outputs.Unpack(res)
	if err != nil || len(outs) == 0 {
		if err == nil {
			err = fmt.Errorf("empty decimals output")
		}
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

func ToFloat(amount *big.Int, decimals int) float64 {
	if amount == nil {
		return 0
	}
	f := new(big.Float).SetInt(amount)
	f.Quo(f, big.NewFloat(math.Pow10(decimals)))
	val, _ := f.Float64()
	return val
}