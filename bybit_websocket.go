package bybit_connector

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type MessageHandler func(message string) error
type ReconnectHandler func() error

func (b *WebSocket) handleIncomingMessages() {
	for {
		_, message, err := b.conn.ReadMessage()
		if err != nil {
			fmt.Println("Error reading:", err)
			b.isConnected = false
			b.conn.Close()
			return
		}

		if b.onMessage != nil {
			err := b.onMessage(string(message))
			if err != nil {
				fmt.Println("Error handling message:", err)
				return
			}
		}
	}
}

func (b *WebSocket) monitorConnection() {
	ticker := time.NewTicker(time.Second * 5) // Check every 5 seconds
	defer ticker.Stop()

	for {
		<-ticker.C
		if !b.isConnected && b.ctx.Err() == nil { // Check if disconnected and context not done
			fmt.Println("Attempting to reconnect...")
			con := b.Connect()
			if con == nil {
				fmt.Println("Reconnection failed:")
			} else {
				fmt.Println("Reconnection successful")
				// Call reconnect handler if set
				if b.onReconnect != nil {
					if err := b.onReconnect(); err != nil {
						fmt.Println("Error in reconnect handler:", err)
					}
				}
			}
		}

		select {
		case <-b.ctx.Done():
			return
		default:
		}
	}
}

func (b *WebSocket) SetMessageHandler(handler MessageHandler) {
	b.onMessage = handler
}

func (b *WebSocket) SetReconnectHandler(handler ReconnectHandler) {
	b.onReconnect = handler
}

type WebSocket struct {
	conn         *websocket.Conn
	url          string
	apiKey       string
	apiSecret    string
	maxAliveTime string
	pingInterval int
	onMessage    MessageHandler
	onReconnect  ReconnectHandler
	ctx          context.Context
	cancel       context.CancelFunc
	isConnected  bool

	// New fields for writer pattern
	writeChan chan []byte
	writeDone chan struct{}
}

type WebsocketOption func(*WebSocket)

func WithPingInterval(pingInterval int) WebsocketOption {
	return func(c *WebSocket) {
		c.pingInterval = pingInterval
	}
}

func WithMaxAliveTime(maxAliveTime string) WebsocketOption {
	return func(c *WebSocket) {
		c.maxAliveTime = maxAliveTime
	}
}

func NewBybitPrivateWebSocket(url, apiKey, apiSecret string, handler MessageHandler, options ...WebsocketOption) *WebSocket {
	c := &WebSocket{
		url:          url,
		apiKey:       apiKey,
		apiSecret:    apiSecret,
		maxAliveTime: "",
		pingInterval: 20,
		onMessage:    handler,
	}

	// Apply the provided options
	for _, opt := range options {
		opt(c)
	}

	return c
}

func NewBybitPublicWebSocket(url string, handler MessageHandler) *WebSocket {
	c := &WebSocket{
		url:          url,
		pingInterval: 20, // default is 20 seconds
		onMessage:    handler,
	}

	return c
}

func (b *WebSocket) Connect() *WebSocket {
	// Stop any existing loops first
	if b.cancel != nil {
		b.cancel()
		// Wait for writer to finish
		select {
		case <-b.writeDone:
		case <-time.After(2 * time.Second):
		}
	}

	var err error
	wssUrl := b.url
	if b.maxAliveTime != "" {
		wssUrl += "?max_alive_time=" + b.maxAliveTime
	}
	b.conn, _, err = websocket.DefaultDialer.Dial(wssUrl, nil)
	if err != nil {
		fmt.Println("Failed to connect to WebSocket:", err)
		return nil
	}

	// Initialize writer pattern BEFORE authentication
	b.writeChan = make(chan []byte, 100)
	b.writeDone = make(chan struct{})
	b.ctx, b.cancel = context.WithCancel(context.Background())

	// Start writer goroutine BEFORE authentication
	go b.writer()

	if b.requiresAuthentication() {
		if err = b.sendAuth(); err != nil {
			fmt.Println("Failed Connection:", fmt.Sprintf("%v", err))
			return nil
		}
	}
	b.isConnected = true

	// Start remaining goroutines
	go b.handleIncomingMessages()
	go b.monitorConnection()
	go b.pinger() // Ping goroutine that sends to writeChan

	return b
}

// Single writer goroutine - no race conditions possible
func (b *WebSocket) writer() {
	defer close(b.writeDone)

	for {
		select {
		case data := <-b.writeChan:
			if err := b.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				fmt.Println("Failed to write message:", err)
				b.isConnected = false
				b.conn.Close()
				return
			}
		case <-b.ctx.Done():
			return
		}
	}
}

// Ping goroutine that sends to write channel
func (b *WebSocket) pinger() {
	if b.pingInterval <= 0 {
		return
	}

	ticker := time.NewTicker(time.Duration(b.pingInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			currentTime := time.Now().Unix()
			pingMessage := map[string]string{
				"op":     "ping",
				"req_id": fmt.Sprintf("%d", currentTime),
			}
			jsonPingMessage, err := json.Marshal(pingMessage)
			if err != nil {
				fmt.Println("Failed to marshal ping message:", err)
				continue
			}

			// Send to writer channel (non-blocking)
			select {
			case b.writeChan <- jsonPingMessage:
				fmt.Println("Ping sent successfully.")
			case <-b.ctx.Done():
				return
			}
		case <-b.ctx.Done():
			return
		}
	}
}

// Updated send method
func (b *WebSocket) send(message string) error {
	select {
	case b.writeChan <- []byte(message):
		return nil
	case <-b.ctx.Done():
		return fmt.Errorf("websocket closed")
	default:
		return fmt.Errorf("write channel full")
	}
}

func (b *WebSocket) SendSubscription(args []string) (*WebSocket, error) {
	reqID := uuid.New().String()
	subMessage := map[string]interface{}{
		"req_id": reqID,
		"op":     "subscribe",
		"args":   args,
	}
	fmt.Println("subscribe msg:", fmt.Sprintf("%v", subMessage["args"]))
	if err := b.sendAsJson(subMessage); err != nil {
		fmt.Println("Failed to send subscription:", err)
		return b, err
	}
	fmt.Println("Subscription sent successfully.")
	return b, nil
}

// SendRequest sendRequest sends a custom request over the WebSocket connection.
func (b *WebSocket) SendRequest(op string, args map[string]interface{}, headers map[string]string, reqId ...string) (*WebSocket, error) {
	finalReqId := uuid.New().String()
	if len(reqId) > 0 && reqId[0] != "" {
		finalReqId = reqId[0]
	}

	request := map[string]interface{}{
		"reqId":  finalReqId,
		"header": headers,
		"op":     op,
		"args":   []interface{}{args},
	}
	fmt.Println("request headers:", fmt.Sprintf("%v", request["header"]))
	fmt.Println("request op channel:", fmt.Sprintf("%v", request["op"]))
	fmt.Println("request msg:", fmt.Sprintf("%v", request["args"]))
	if err := b.sendAsJson(request); err != nil {
		fmt.Println("Failed to send websocket trade request:", err)
		return b, err
	}
	fmt.Println("Successfully sent websocket trade request.")
	return b, nil
}

func (b *WebSocket) SendTradeRequest(tradeTruest map[string]interface{}) (*WebSocket, error) {
	fmt.Println("trade request headers:", fmt.Sprintf("%v", tradeTruest["header"]))
	fmt.Println("trade request op channel:", fmt.Sprintf("%v", tradeTruest["op"]))
	fmt.Println("trade request msg:", fmt.Sprintf("%v", tradeTruest["args"]))
	if err := b.sendAsJson(tradeTruest); err != nil {
		fmt.Println("Failed to send websocket trade request:", err)
		return b, err
	}
	fmt.Println("Successfully sent websocket trade request.")
	return b, nil
}

func (b *WebSocket) Disconnect() error {
	b.cancel()
	b.isConnected = false
	return b.conn.Close()
}

func (b *WebSocket) requiresAuthentication() bool {
	return b.url == WEBSOCKET_PRIVATE_MAINNET || b.url == WEBSOCKET_PRIVATE_TESTNET ||
		b.url == WEBSOCKET_TRADE_MAINNET || b.url == WEBSOCKET_TRADE_TESTNET ||
		b.url == WEBSOCKET_TRADE_DEMO || b.url == WEBSOCKET_PRIVATE_DEMO
	// v3 offline
	/*
		b.url == V3_CONTRACT_PRIVATE ||
			b.url == V3_UNIFIED_PRIVATE ||
			b.url == V3_SPOT_PRIVATE
	*/
}

func (b *WebSocket) sendAuth() error {
	// Get current Unix time in milliseconds
	expires := time.Now().UnixNano()/1e6 + 10000
	val := fmt.Sprintf("GET/realtime%d", expires)

	h := hmac.New(sha256.New, []byte(b.apiSecret))
	h.Write([]byte(val))

	// Convert to hexadecimal instead of base64
	signature := hex.EncodeToString(h.Sum(nil))
	fmt.Println("signature generated : " + signature)

	authMessage := map[string]interface{}{
		"req_id": uuid.New(),
		"op":     "auth",
		"args":   []interface{}{b.apiKey, expires, signature},
	}
	fmt.Println("auth args:", fmt.Sprintf("%v", authMessage["args"]))
	return b.sendAsJson(authMessage)
}

func (b *WebSocket) sendAsJson(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.send(string(data))
}
