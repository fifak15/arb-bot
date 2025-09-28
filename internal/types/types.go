package types

import "time"

type Opportunity struct{ QtyBase, BuyPxCEX, DexOutUSD, GasUSD, NetUSD, ROI float64; Ts time.Time }
