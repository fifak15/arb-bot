package mexc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/you/arb-bot/internal/config"
	"go.uber.org/zap"
)

type Client struct {
	cfg  *config.Config
	log  *zap.Logger
	http *http.Client
}

func NewClient(cfg *config.Config, log *zap.Logger) (*Client, error) {
	return &Client{cfg: cfg, log: log, http: &http.Client{Timeout: 6 * time.Second}}, nil
}

type bookTickerResp struct {
	Symbol   string `json:"symbol"`
	BidPrice string `json:"bidPrice"`
	AskPrice string `json:"askPrice"`
}

func (c *Client) BestBidAsk(symbol string) (bid, ask float64, err error) {
	endpoint := c.cfg.MEXC.RestURL + "/api/v3/ticker/bookTicker?symbol=" + url.QueryEscape(symbol)
	req, _ := http.NewRequest("GET", endpoint, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("bookTicker %d: %s", resp.StatusCode, string(b))
	}
	var br bookTickerResp
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		return 0, 0, err
	}
	var bpf, apf float64
	fmt.Sscan(br.BidPrice, &bpf)
	fmt.Sscan(br.AskPrice, &apf)
	return bpf, apf, nil
}

func (c *Client) PlaceIOC(ctx context.Context, symbol, side string, qty, price float64) (orderID string, filledQty, avgPrice float64, err error) {
	ts := time.Now().UnixMilli()
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", side)
	params.Set("type", "LIMIT")
	params.Set("quantity", trim(qty))
	params.Set("price", trim(price))
	params.Set("timeInForce", "IOC")
	params.Set("timestamp", fmt.Sprintf("%d", ts))
	params.Set("recvWindow", "5000")
	params.Set("signature", c.sign(params.Encode()))

	endpoint := c.cfg.MEXC.RestURL + "/api/v3/order"
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(params.Encode()))
	req.Header.Set("X-MEXC-APIKEY", c.cfg.MEXC.ApiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", 0, 0, fmt.Errorf("order %d: %s", resp.StatusCode, string(body))
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", 0, 0, err
	}
	switch v := obj["orderId"].(type) {
	case string:
		orderID = v
	case float64:
		orderID = fmt.Sprintf("%.0f", v)
	default:
	}

	q := url.Values{}
	q.Set("symbol", symbol)
	q.Set("orderId", orderID)
	q.Set("timestamp", fmt.Sprintf("%d", time.Now().UnixMilli()))
	q.Set("recvWindow", "5000")
	q.Set("signature", c.sign(q.Encode()))

	ordURL := c.cfg.MEXC.RestURL + "/api/v3/order?" + q.Encode()
	oreq, _ := http.NewRequestWithContext(ctx, "GET", ordURL, nil)
	oreq.Header.Set("X-MEXC-APIKEY", c.cfg.MEXC.ApiKey)

	oresp, err := c.http.Do(oreq)
	if err != nil {
		return orderID, 0, 0, fmt.Errorf("order query: %w", err)
	}
	defer oresp.Body.Close()
	obody, _ := io.ReadAll(oresp.Body)
	if oresp.StatusCode != 200 {
		return orderID, 0, 0, fmt.Errorf("order query %d: %s", oresp.StatusCode, string(obody))
	}

	var ord map[string]any
	if err := json.Unmarshal(obody, &ord); err != nil {
		return orderID, 0, 0, err
	}

	var execQty, cummQuote float64
	if s, ok := ord["executedQty"].(string); ok {
		fmt.Sscan(s, &execQty)
	}
	if s, ok := ord["cummulativeQuoteQty"].(string); ok {
		fmt.Sscan(s, &cummQuote)
	}
	status, _ := ord["status"].(string)

	if execQty <= 0 {
		c.log.Warn("IOC: исполнение отсутствует — ордер отменён", zap.String("status", status), zap.String("order_id", orderID), zap.String("symbol", symbol))
		return orderID, 0, 0, nil
	}
	if execQty > 0 {
		avgPrice = cummQuote / execQty
	}
	c.log.Info("IOC: исполнение успешно",
		zap.String("order_id", orderID),
		zap.String("status", status),
		zap.String("symbol", symbol),
		zap.Float64("executed_qty", execQty),
		zap.Float64("avg_price", avgPrice),
	)
	return orderID, execQty, avgPrice, nil
}

func (c *Client) sign(q string) string {
	mac := hmac.New(sha256.New, []byte(c.cfg.MEXC.ApiSecret))
	mac.Write([]byte(q))
	return hex.EncodeToString(mac.Sum(nil))
}

func trim(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.8f", v), "0"), ".")
}
