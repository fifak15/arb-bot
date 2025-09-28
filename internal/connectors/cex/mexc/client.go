
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

    "go.uber.org/zap"
    "github.com/you/arb-bot/internal/config"
)

type Client struct {
    cfg *config.Config
    log *zap.Logger
    http *http.Client
}

func NewClient(cfg *config.Config, log *zap.Logger) (*Client, error) {
    return &Client{cfg: cfg, log: log, http: &http.Client{Timeout: 6 * time.Second}}, nil
}

type bookTickerResp struct {
    Symbol string `json:"symbol"`
    BidPrice string `json:"bidPrice"`
    AskPrice string `json:"askPrice"`
}

func (c *Client) BestBidAsk(symbol string) (bid, ask float64, err error) {
    endpoint := c.cfg.MEXC.RestURL + "/api/v3/ticker/bookTicker?symbol=" + url.QueryEscape(symbol)
    req, _ := http.NewRequest("GET", endpoint, nil)
    resp, err := c.http.Do(req)
    if err != nil { return 0,0, err }
    defer resp.Body.Close()
    if resp.StatusCode != 200 { b, _ := io.ReadAll(resp.Body); return 0,0, fmt.Errorf("bookTicker %d: %s", resp.StatusCode, string(b)) }
    var br bookTickerResp
    if err := json.NewDecoder(resp.Body).Decode(&br); err != nil { return 0,0, err }
    var bpf, apf float64
    fmt.Sscan(br.BidPrice, &bpf)
    fmt.Sscan(br.AskPrice, &apf)
    return bpf, apf, nil
}

func (c *Client) PlaceIOC(ctx context.Context, symbol string, side string, qty float64, price float64) (orderID string, filledQty, avgPrice float64, err error) {
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
    sig := c.sign(params.Encode())
    params.Set("signature", sig)

    endpoint := c.cfg.MEXC.RestURL + "/api/v3/order"
    req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(params.Encode()))
    req.Header.Set("X-MEXC-APIKEY", c.cfg.MEXC.ApiKey)
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
    resp, err := c.http.Do(req)
    if err != nil { return "",0,0, err }
    defer resp.Body.Close()
    body, _ := io.ReadAll(resp.Body)
    if resp.StatusCode != 200 { return "",0,0, fmt.Errorf("order %d: %s", resp.StatusCode, string(body)) }
    var obj map[string]any
    if err := json.Unmarshal(body, &obj); err != nil { return "",0,0, err }
    oid := ""
    if s, ok := obj["orderId"].(string); ok { oid = s } else if f, ok := obj["orderId"].(float64); ok { oid = fmt.Sprintf("%.0f", f) }
    return oid, qty, price, nil
}

func (c *Client) sign(q string) string {
    mac := hmac.New(sha256.New, []byte(c.cfg.MEXC.ApiSecret))
    mac.Write([]byte(q))
    return hex.EncodeToString(mac.Sum(nil))
}

func trim(v float64) string { return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.8f", v), "0"), ".") }
