
# arb-bot (real)

- MEXC REST: `/api/v3/ticker/bookTicker`, `/api/v3/order` (IOC LIMIT).
- Uniswap v3 on-chain: QuoterV2 `quoteExactInputSingle`, Router `exactInputSingle`.

Fill `config.yaml`, then:
```bash
go mod tidy
go run ./cmd/arb-bot -config ./config.yaml
```
**Важно:** кошелёк должен иметь WETH и ETH на газ. Код минимален и без production-защит.
