package multicall

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const multicallABI = `[
{
    "constant": false,
    "inputs": [
        {
            "components": [
                {
                    "name": "target",
                    "type": "address"
                },
                {
                    "name": "callData",
                    "type": "bytes"
                }
            ],
            "name": "calls",
            "type": "tuple[]"
        }
    ],
    "name": "aggregate",
    "outputs": [
        {
            "name": "blockNumber",
            "type": "uint256"
        },
        {
            "name": "returnData",
            "type": "bytes[]"
        }
    ],
    "payable": false,
    "stateMutability": "nonpayable",
    "type": "function"
}
]`

type IClient interface {
	Aggregate(ctx context.Context, calls []Call) ([]Result, error)
}

type Client struct {
	c    *ethclient.Client
	addr common.Address
	abi  abi.ABI
}

func New(c *ethclient.Client, multicallAddr common.Address) (IClient, error) {
	parsedABI, err := abi.JSON(strings.NewReader(multicallABI))
	if err != nil {
		return nil, fmt.Errorf("bad abi: %w", err)
	}
	return &Client{c: c, addr: multicallAddr, abi: parsedABI}, nil
}

type Call struct {
	Target   common.Address
	CallData []byte
}

type Result struct {
	Success bool
	Data    []byte
}

func (c *Client) Aggregate(ctx context.Context, calls []Call) ([]Result, error) {
	payload, err := c.abi.Pack("aggregate", calls)
	if err != nil {
		return nil, fmt.Errorf("pack aggregate: %w", err)
	}

	res, err := c.c.CallContract(ctx, ethereum.CallMsg{To: &c.addr, Data: payload}, nil)
	if err != nil {
		return nil, fmt.Errorf("call aggregate: %w", err)
	}

	type AggregateResult struct {
		BlockNumber *big.Int
		ReturnData  [][]byte
	}
	var aggRes AggregateResult
	if err := c.abi.UnpackIntoInterface(&aggRes, "aggregate", res); err != nil {
		return nil, fmt.Errorf("unpack aggregate: %w", err)
	}

	out := make([]Result, len(calls))
	for i, r := range aggRes.ReturnData {
		out[i] = Result{Success: len(r) > 0, Data: r}
	}
	return out, nil
}