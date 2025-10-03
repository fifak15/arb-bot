package mexc

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/you/arb-bot/internal/connectors/cex/mexc/pb"
	"google.golang.org/protobuf/proto"
)

type MiniTicker struct {
	Symbol   string
	Price    string
	Volume   string
	High     string
	Low      string
	Rate     string
	SendTime int64
}

type MiniWS struct {
	URL    string
	Dialer *websocket.Dialer
	conn   *websocket.Conn
	mu     sync.Mutex
}

func NewMiniWS(url string) *MiniWS {
	return &MiniWS{
		URL: strings.TrimRight(url, "/"),
		Dialer: &websocket.Dialer{
			HandshakeTimeout:  15 * time.Second,
			EnableCompression: true, // включает permessage-deflate на уровне WS
		},
	}
}

func (w *MiniWS) connect(ctx context.Context) error {
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
	_ = w.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	w.conn.SetPongHandler(func(string) error {
		// двигаем дедлайн на каждый PONG
		return w.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	})
	return nil
}

func (w *MiniWS) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil {
		return w.conn.Close()
	}
	return nil
}

// tryDecompress пытается распаковать gzip/zlib, иначе возвращает исходные байты.
func tryDecompress(b []byte) []byte {
	// gzip signature 0x1f 0x8b
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		if r, err := gzip.NewReader(bytes.NewReader(b)); err == nil {
			defer r.Close()
			if out, err := io.ReadAll(r); err == nil {
				return out
			}
		}
	}
	// частый zlib (deflate) с заголовком, обычно начинается на 0x78
	if len(b) > 2 && b[0] == 0x78 {
		if r, err := zlib.NewReader(bytes.NewReader(b)); err == nil {
			defer r.Close()
			if out, err := io.ReadAll(r); err == nil {
				return out
			}
		}
	}
	return b
}

func (w *MiniWS) SubscribeMiniTickers(ctx context.Context) (<-chan MiniTicker, <-chan error, error) {
	if err := w.connect(ctx); err != nil {
		return nil, nil, err
	}

	sub := struct {
		ID     int      `json:"id"`
		Method string   `json:"method"`
		Params []string `json:"params"`
	}{ID: 1, Method: "SUBSCRIPTION", Params: []string{"spot@public.miniTickers.v3.api.pb@UTC+0"}}

	if err := w.conn.WriteJSON(sub); err != nil {
		return nil, nil, err
	}

	out := make(chan MiniTicker, 2048)
	errc := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errc)
		defer w.Close()

		// поддержание соединения: PING каждые 20с
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
			Method  string `json:"method,omitempty"`
		}

		// более стабильная проверка канала: без жёсткого @UTC+0
		const chanCore = "spot@public.miniTickers.v3.api.pb"

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			msgType, data, err := w.conn.ReadMessage()
			if err != nil {
				errc <- err
				return
			}
			_ = w.conn.SetReadDeadline(time.Now().Add(90 * time.Second))

			// текстовые ответы: ack, ping/pong и т.п.
			if msgType == websocket.TextMessage {
				var a ack
				_ = json.Unmarshal(data, &a) // ack не критичен; ошибок не боимся
				// можно обработать серверный PING -> PONG (на всякий)
				if strings.EqualFold(a.Method, "PING") {
					_ = w.conn.WriteMessage(websocket.TextMessage, []byte(`{"method":"PONG"}`))
				}
				continue
			}
			if msgType != websocket.BinaryMessage {
				continue
			}

			// бинарное сообщение — это protobuf PushDataV3ApiWrapper
			raw := tryDecompress(data)

			var wrap pb.PushDataV3ApiWrapper
			if err := proto.Unmarshal(raw, &wrap); err != nil {
				// не удалось распарсить — пропускаем кадр
				continue
			}

			// допускаем нестрогое соответствие каналу, но фильтруем явные лишние
			ch := wrap.GetChannel()
			if ch != "" && !strings.Contains(ch, chanCore) {
				// если канал странный, но дальше нет тела miniTickers — пропускаем;
				// если тело есть — всё равно обработаем ниже
				// continue — НЕ делаем здесь
			}

			m := wrap.GetPublicMiniTickers()
			if m == nil {
				continue
			}

			// общее время отправки для батча
			ts := time.Now()
			if wrap.GetSendTime() > 0 {
				ts = time.UnixMilli(wrap.GetSendTime())
			}

			for _, it := range m.GetItems() {
				price, err1 := strconv.ParseFloat(it.GetPrice(), 64)
				vol, err2 := strconv.ParseFloat(it.GetVolume(), 64)
				if err1 != nil || err2 != nil || price <= 0 || vol <= 0 {
					continue
				}
				select {
				case out <- MiniTicker{
					Symbol:   it.GetSymbol(),
					Price:    it.GetPrice(),
					Volume:   it.GetVolume(),
					High:     it.GetHigh(),
					Low:      it.GetLow(),
					Rate:     it.GetRate(),
					SendTime: ts.UnixMilli(),
				}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, errc, nil
}
