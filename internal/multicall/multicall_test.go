package multicall

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const erc20ABI = `[{"constant":true,"inputs":[],"name":"name","outputs":[{"name":"","type":"string"}],"payable":false,"stateMutability":"view","type":"function"}]`

func TestAggregate(t *testing.T) {
	// This is an integration test that requires a running RPC node with a deployed Multicall contract.
	// For now, we will just test the packing and unpacking of the data.
	t.Skip("skipping integration test for now")

	// To run this test, you will need to set the following variables:
	// rpcURL := "YOUR_RPC_URL"
	// multicallAddr := common.HexToAddress("YOUR_MULTICALL_ADDRESS")
	// tokenAddr := common.HexToAddress("A_TOKEN_ADDRESS_WITH_A_NAME")

	// ec, err := ethclient.Dial(rpcURL)
	// require.NoError(t, err)

	// mc, err := New(ec, multicallAddr)
	// require.NoError(t, err)

	// erc20, err := abi.JSON(strings.NewReader(erc20ABI))
	// require.NoError(t, err)

	// callData, err := erc20.Pack("name")
	// require.NoError(t, err)

	// calls := []Call{
	// 	{
	// 		Target:   tokenAddr,
	// 		CallData: callData,
	// 	},
	// }

	// results, err := mc.Aggregate(context.Background(), calls)
	// require.NoError(t, err)
	// require.Len(t, results, 1)
	// assert.True(t, results[0].Success)

	// var name string
	// err = erc20.UnpackIntoInterface(&name, "name", results[0].Data)
	// require.NoError(t, err)
	// assert.NotEmpty(t, name)
	// t.Logf("token name: %s", name)
}

func TestABIPacking(t *testing.T) {
	erc20, err := abi.JSON(strings.NewReader(erc20ABI))
	require.NoError(t, err)

	callData, err := erc20.Pack("name")
	require.NoError(t, err)
	assert.NotEmpty(t, callData)
}