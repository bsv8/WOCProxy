package whatsonchain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetAddressSpendableUnspentIncludesUnconfirmedAndSkipsMempoolSpent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/address/testaddr/unspent/all":
			_, _ = w.Write([]byte(`{"result":[
				{"tx_hash":"confirmed1","tx_pos":0,"value":10,"isSpentInMempoolTx":false,"status":"confirmed"},
				{"tx_hash":"unconfirmed1","tx_pos":1,"value":20,"isSpentInMempoolTx":false,"status":"unconfirmed"},
				{"tx_hash":"mempool_spent","tx_pos":2,"value":30,"isSpentInMempoolTx":true,"status":"confirmed"}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, AuthConfig{})
	utxos, err := c.GetAddressSpendableUnspent(context.Background(), "testaddr")
	if err != nil {
		t.Fatalf("GetAddressSpendableUnspent() error: %v", err)
	}
	if len(utxos) != 2 {
		t.Fatalf("utxo count mismatch: got=%d want=2", len(utxos))
	}
	if utxos[0].TxID != "confirmed1" || utxos[1].TxID != "unconfirmed1" {
		t.Fatalf("unexpected utxos: %+v", utxos)
	}
}

func TestGetAddressConfirmedUnspentSkipsUnconfirmedFromWrappedPayload(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/address/testaddr/confirmed/unspent":
			_, _ = w.Write([]byte(`{"result":[
				{"tx_hash":"confirmed1","tx_pos":0,"value":10,"isSpentInMempoolTx":false,"status":"confirmed"},
				{"tx_hash":"unconfirmed1","tx_pos":1,"value":20,"isSpentInMempoolTx":false,"status":"unconfirmed"}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, AuthConfig{})
	utxos, err := c.GetAddressConfirmedUnspent(context.Background(), "testaddr")
	if err != nil {
		t.Fatalf("GetAddressConfirmedUnspent() error: %v", err)
	}
	if len(utxos) != 1 {
		t.Fatalf("utxo count mismatch: got=%d want=1", len(utxos))
	}
	if utxos[0].TxID != "confirmed1" {
		t.Fatalf("unexpected utxo: %+v", utxos[0])
	}
}
