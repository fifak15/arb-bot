package mexc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	pb "github.com/you/arb-bot/internal/connectors/cex/mexc/pb"
	"google.golang.org/protobuf/proto"
)

type Ticker struct {
	Symbol string
	Bid    float64
	Ask    float64
	TS     time.Time
}

type WS struct {
	URL    string
	Dialer *websocket.Dialer
	conn   *websocket.Conn
	mu     sync.Mutex
}

func NewWS(url string) *WS {
	return &WS{
		URL: strings.TrimRight(url, "/"),
		Dialer: &websocket.Dialer{
			HandshakeTimeout:  15 * time.Second,
			EnableCompression: true,
		},
	}
}

func (w *WS) connect(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil {
		return nil
	}
	h := http.Header{"Origin": []string{"https://www.mexc.com"}}
	c, _, err := w.Dialer.DialContext(ctx, w.URL, h)
	if err != nil {
		return err
	}
	w.conn = c

	// опционально: set read deadline + pong handler на уровне WS-контроля
	_ = w.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	w.conn.SetPongHandler(func(string) error {
		return w.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	})

	return nil
}

func (w *WS) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil {
		return w.conn.Close()
	}
	return nil
}

func (w *WS) SubscribeBookTicker(ctx context.Context, symbols []string) (<-chan Ticker, error) {
	if err := w.connect(ctx); err != nil {
		return nil, err
	}

	params := make([]string, 0, len(symbols))
	for _, s := range symbols {
		params = append(params, "spot@public.aggre.bookTicker.v3.api.pb@100ms@"+strings.ToUpper(s))
	}
	sub := struct {
		ID     int      `json:"id"`
		Method string   `json:"method"`
		Params []string `json:"params"`
	}{ID: 1, Method: "SUBSCRIPTION", Params: params}

	if err := w.conn.WriteJSON(sub); err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	out := make(chan Ticker, 1024)

	go func() {
		defer close(out)
		defer w.Close()

		pingStop := make(chan struct{})
		go func() {
			t := time.NewTicker(20 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-pingStop:
					return
				case <-t.C:
					_ = w.conn.WriteMessage(websocket.TextMessage, []byte(`{"method":"PING"}`))
				}
			}
		}()
		defer close(pingStop)

		type ack struct {
			ID      *int   `json:"id,omitempty"`
			Code    *int   `json:"code,omitempty"`
			Msg     string `json:"msg,omitempty"`
			Channel string `json:"channel,omitempty"`
		}

		const chanPrefix = "spot@public.aggre.bookTicker.v3.api.pb@"

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			msgType, data, err := w.conn.ReadMessage()
			if err != nil {
				return
			}

			_ = w.conn.SetReadDeadline(time.Now().Add(90 * time.Second))

			if msgType == websocket.TextMessage {
				var a ack
				if json.Unmarshal(data, &a) == nil {
					if a.ID != nil || strings.EqualFold(a.Msg, "PONG") {
						continue
					}
					continue
				}
				continue
			}

			if msgType != websocket.BinaryMessage {
				continue
			}

			var wrap pb.PushDataV3ApiWrapper
			if err := proto.Unmarshal(data, &wrap); err != nil {
				continue
			}

			ch := wrap.GetChannel()
			if !strings.HasPrefix(ch, chanPrefix) {
				continue
			}

			bt := wrap.GetPublicAggreBookTicker()
			if bt == nil {
				continue
			}

			var bid, ask float64
			if v, err := strconv.ParseFloat(bt.GetBidPrice(), 64); err == nil {
				bid = v
			}
			if v, err := strconv.ParseFloat(bt.GetAskPrice(), 64); err == nil {
				ask = v
			}
			if bid == 0 && ask == 0 {
				continue
			}

			ts := time.Now()
			if wrap.GetSendTime() > 0 {
				ts = time.UnixMilli(wrap.GetSendTime())
			}

			out <- Ticker{
				Symbol: wrap.GetSymbol(),
				Bid:    bid,
				Ask:    ask,
				TS:     ts,
			}
		}
	}()

	return out, nil
}
