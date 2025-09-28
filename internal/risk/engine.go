package risk

import "github.com/you/arb-bot/internal/config"

type Engine struct{ cfg *config.Config }
func NewEngine(cfg *config.Config)*Engine{ return &Engine{cfg:cfg} }
func (e *Engine) AllowTrade(netUSD, roi float64) bool { if netUSD<e.cfg.Risk.MinProfitUSD {return false}; if roi<e.cfg.Risk.MinROIBps/10000.0 {return false}; return true }
