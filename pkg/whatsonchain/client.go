package whatsonchain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	TestnetBaseURL = "https://api.whatsonchain.com/v1/bsv/test"
	MainnetBaseURL = "https://api.whatsonchain.com/v1/bsv/main"
)

func BaseURLForNetwork(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "main":
		return MainnetBaseURL
	default:
		return TestnetBaseURL
	}
}

type Client struct {
	baseURL string
	auth    AuthConfig
	http    *http.Client
}

type AuthConfig struct {
	Mode  string
	Name  string
	Value string
}

type UTXO struct {
	TxID  string
	Vout  uint32
	Value uint64
}

type AddressHistoryItem struct {
	TxID   string
	Height int64
}

type ConfirmedHistoryQuery struct {
	Order  string
	Limit  int
	Height int64
	Token  string
}

type ConfirmedHistoryPage struct {
	Items         []AddressHistoryItem
	NextPageToken string
}

type TxDetail struct {
	TxID string
	Vin  []TxInput
	Vout []TxOutput
}

type TxInput struct {
	TxID string
	Vout uint32
}

type TxOutput struct {
	N            uint32
	Value        float64
	ValueSatoshi uint64
	ScriptPubKey ScriptPubKey
}

type ScriptPubKey struct {
	Hex string
}

type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "http error"
	}
	return fmt.Sprintf("http %d: %s", e.StatusCode, e.Body)
}

func (e *HTTPError) HTTPStatus() int {
	if e == nil {
		return 0
	}
	return e.StatusCode
}

func (e *HTTPError) HTTPBody() string {
	if e == nil {
		return ""
	}
	return e.Body
}

func NewClient(baseURL string, auth AuthConfig) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = TestnetBaseURL
	}
	return &Client{
		baseURL: baseURL,
		auth:    auth,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

func (c *Client) GetAddressConfirmedUnspent(ctx context.Context, address string) ([]UTXO, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, fmt.Errorf("address is required")
	}
	body, err := c.get(ctx, "/address/"+address+"/confirmed/unspent")
	if err != nil {
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
			return nil, err
		}
		body, err = c.get(ctx, "/address/"+address+"/unspent")
		if err != nil {
			return nil, err
		}
	}
	var raw []struct {
		TxID               string `json:"tx_hash"`
		Vout               uint32 `json:"tx_pos"`
		Value              uint64 `json:"value"`
		IsSpentInMempoolTx bool   `json:"isSpentInMempoolTx"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		var wrapped struct {
			Result []struct {
				TxID               string `json:"tx_hash"`
				Vout               uint32 `json:"tx_pos"`
				Value              uint64 `json:"value"`
				IsSpentInMempoolTx bool   `json:"isSpentInMempoolTx"`
			} `json:"result"`
		}
		if wrapErr := json.Unmarshal(body, &wrapped); wrapErr != nil {
			return nil, fmt.Errorf("decode utxos: %w", err)
		}
		raw = wrapped.Result
	}
	out := make([]UTXO, 0, len(raw))
	for _, item := range raw {
		if item.IsSpentInMempoolTx {
			continue
		}
		out = append(out, UTXO{
			TxID:  strings.TrimSpace(item.TxID),
			Vout:  item.Vout,
			Value: item.Value,
		})
	}
	return out, nil
}

func (c *Client) GetChainInfo(ctx context.Context) (uint32, error) {
	body, err := c.get(ctx, "/chain/info")
	if err != nil {
		return 0, err
	}
	var info struct {
		Blocks uint32 `json:"blocks"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return 0, fmt.Errorf("decode chain info: %w", err)
	}
	return info.Blocks, nil
}

func (c *Client) GetAddressConfirmedHistory(ctx context.Context, address string) ([]AddressHistoryItem, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, fmt.Errorf("address is required")
	}
	body, err := c.get(ctx, "/address/"+address+"/confirmed/history")
	if err != nil {
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
			return nil, err
		}
		body, err = c.get(ctx, "/address/"+address+"/history")
		if err != nil {
			return nil, err
		}
	}
	return decodeAddressHistory(body)
}

func (c *Client) GetAddressConfirmedHistoryPage(ctx context.Context, address string, q ConfirmedHistoryQuery) (ConfirmedHistoryPage, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return ConfirmedHistoryPage{}, fmt.Errorf("address is required")
	}
	body, err := c.get(ctx, "/address/"+address+"/confirmed/history"+buildConfirmedHistoryQuery(q))
	if err != nil {
		return ConfirmedHistoryPage{}, err
	}
	return decodeConfirmedHistoryPage(body)
}

func (c *Client) GetAddressUnconfirmedHistory(ctx context.Context, address string) ([]string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, fmt.Errorf("address is required")
	}
	body, err := c.get(ctx, "/address/"+address+"/unconfirmed/history")
	if err != nil {
		return nil, err
	}
	return decodeUnconfirmedHistory(body)
}

func (c *Client) PostTxRaw(ctx context.Context, txHex string) (string, error) {
	txHex = strings.TrimSpace(txHex)
	if txHex == "" {
		return "", fmt.Errorf("tx_hex is required")
	}
	body, err := c.postJSON(ctx, "/tx/raw", map[string]string{"txhex": txHex})
	if err != nil {
		return "", err
	}
	var txid string
	if err := json.Unmarshal(body, &txid); err == nil && strings.TrimSpace(txid) != "" {
		return strings.TrimSpace(txid), nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err == nil {
		if v, ok := obj["txid"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), nil
		}
		if v, ok := obj["data"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("unexpected broadcast response: %s", strings.TrimSpace(string(body)))
}

func (c *Client) GetTxHash(ctx context.Context, txid string) (TxDetail, error) {
	txid = strings.TrimSpace(txid)
	if txid == "" {
		return TxDetail{}, fmt.Errorf("txid is required")
	}
	body, err := c.get(ctx, "/tx/hash/"+txid)
	if err != nil {
		return TxDetail{}, err
	}
	var raw struct {
		TxID string `json:"txid"`
		Vin  []struct {
			TxID string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"vin"`
		Vout []struct {
			N            uint32 `json:"n"`
			Value        any    `json:"value"`
			ScriptPubKey struct {
				Hex string `json:"hex"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return TxDetail{}, fmt.Errorf("decode tx detail: %w", err)
	}
	out := TxDetail{
		TxID: strings.TrimSpace(raw.TxID),
		Vin:  make([]TxInput, 0, len(raw.Vin)),
		Vout: make([]TxOutput, 0, len(raw.Vout)),
	}
	for _, in := range raw.Vin {
		out.Vin = append(out.Vin, TxInput{TxID: strings.TrimSpace(in.TxID), Vout: in.Vout})
	}
	for _, vout := range raw.Vout {
		value, satoshi := normalizeOutputValue(vout.Value)
		out.Vout = append(out.Vout, TxOutput{
			N:            vout.N,
			Value:        value,
			ValueSatoshi: satoshi,
			ScriptPubKey: ScriptPubKey{Hex: strings.TrimSpace(vout.ScriptPubKey.Hex)},
		})
	}
	return out, nil
}

func (a AuthConfig) Apply(req *http.Request) error {
	if req == nil {
		return fmt.Errorf("request is nil")
	}
	mode := strings.ToLower(strings.TrimSpace(a.Mode))
	switch mode {
	case "", "none":
		return nil
	case "header":
		name := strings.TrimSpace(a.Name)
		if name == "" {
			return fmt.Errorf("auth name is required for mode header")
		}
		if strings.TrimSpace(a.Value) == "" {
			return fmt.Errorf("auth value is required for mode header")
		}
		req.Header.Set(name, strings.TrimSpace(a.Value))
		return nil
	case "query":
		name := strings.TrimSpace(a.Name)
		if name == "" {
			return fmt.Errorf("auth name is required for mode query")
		}
		if strings.TrimSpace(a.Value) == "" {
			return fmt.Errorf("auth value is required for mode query")
		}
		q := req.URL.Query()
		q.Set(name, strings.TrimSpace(a.Value))
		req.URL.RawQuery = q.Encode()
		return nil
	case "bearer":
		if strings.TrimSpace(a.Value) == "" {
			return fmt.Errorf("auth value is required for mode bearer")
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(a.Value))
		return nil
	default:
		return fmt.Errorf("unsupported auth mode: %s", mode)
	}
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctxOrBackground(ctx), http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if err := c.auth.Apply(req); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return body, nil
}

func (c *Client) postJSON(ctx context.Context, path string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctxOrBackground(ctx), http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.auth.Apply(req); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return body, nil
}

func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func decodeAddressHistory(body []byte) ([]AddressHistoryItem, error) {
	var raw []struct {
		TxID   string `json:"tx_hash"`
		Height int64  `json:"height"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		var wrapped struct {
			Result []struct {
				TxID   string `json:"tx_hash"`
				Height int64  `json:"height"`
			} `json:"result"`
		}
		if wrapErr := json.Unmarshal(body, &wrapped); wrapErr != nil {
			return nil, fmt.Errorf("decode address history: %w", err)
		}
		raw = wrapped.Result
	}
	out := make([]AddressHistoryItem, 0, len(raw))
	for _, item := range raw {
		txid := strings.TrimSpace(item.TxID)
		if txid == "" {
			continue
		}
		out = append(out, AddressHistoryItem{TxID: txid, Height: item.Height})
	}
	return out, nil
}

func decodeConfirmedHistoryPage(body []byte) (ConfirmedHistoryPage, error) {
	items, err := decodeAddressHistory(body)
	if err != nil {
		return ConfirmedHistoryPage{}, err
	}
	var out struct {
		NextPageToken string `json:"nextPageToken"`
	}
	_ = json.Unmarshal(body, &out)
	if strings.TrimSpace(out.NextPageToken) == "" {
		var wrapped struct {
			NextPageToken string `json:"next_page_token"`
		}
		_ = json.Unmarshal(body, &wrapped)
		out.NextPageToken = wrapped.NextPageToken
	}
	return ConfirmedHistoryPage{
		Items:         items,
		NextPageToken: strings.TrimSpace(out.NextPageToken),
	}, nil
}

func decodeUnconfirmedHistory(body []byte) ([]string, error) {
	var raw []struct {
		TxID string `json:"tx_hash"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		var wrapped struct {
			Result []struct {
				TxID string `json:"tx_hash"`
			} `json:"result"`
		}
		if wrapErr := json.Unmarshal(body, &wrapped); wrapErr != nil {
			return nil, fmt.Errorf("decode unconfirmed history: %w", err)
		}
		raw = wrapped.Result
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		txid := normalizeHexID(item.TxID)
		if txid == "" {
			continue
		}
		out = append(out, txid)
	}
	return out, nil
}

func buildConfirmedHistoryQuery(q ConfirmedHistoryQuery) string {
	params := url.Values{}
	if order := strings.ToLower(strings.TrimSpace(q.Order)); order == "asc" || order == "desc" {
		params.Set("order", order)
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Height > 0 {
		params.Set("height", strconv.FormatInt(q.Height, 10))
	}
	if token := strings.TrimSpace(q.Token); token != "" {
		params.Set("nextPageToken", token)
	}
	if len(params) == 0 {
		return ""
	}
	return "?" + params.Encode()
}

func normalizeOutputValue(raw any) (float64, uint64) {
	switch v := raw.(type) {
	case float64:
		return v, uint64(v*100_000_000 + 0.5)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, 0
		}
		return f, uint64(f*100_000_000 + 0.5)
	default:
		return 0, 0
	}
}

func normalizeHexID(in string) string {
	v := strings.ToLower(strings.TrimSpace(in))
	if len(v) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(v); err != nil {
		return ""
	}
	return v
}
