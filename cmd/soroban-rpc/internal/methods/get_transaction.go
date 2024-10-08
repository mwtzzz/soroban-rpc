package methods

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/creachadair/jrpc2"

	"github.com/stellar/go/support/log"
	"github.com/stellar/go/xdr"

	"github.com/stellar/soroban-rpc/cmd/soroban-rpc/internal/db"
)

const (
	// TransactionStatusSuccess indicates the transaction was included in the ledger and
	// it was executed without errors.
	TransactionStatusSuccess = "SUCCESS"
	// TransactionStatusNotFound indicates the transaction was not found in Soroban-RPC's
	// transaction store.
	TransactionStatusNotFound = "NOT_FOUND"
	// TransactionStatusFailed indicates the transaction was included in the ledger and
	// it was executed with an error.
	TransactionStatusFailed = "FAILED"
)

// GetTransactionResponse is the response for the Soroban-RPC getTransaction() endpoint
type GetTransactionResponse struct {
	// Status is one of: TransactionSuccess, TransactionNotFound, or TransactionFailed.
	Status string `json:"status"`
	// LatestLedger is the latest ledger stored in Soroban-RPC.
	LatestLedger uint32 `json:"latestLedger"`
	// LatestLedgerCloseTime is the unix timestamp of when the latest ledger was closed.
	LatestLedgerCloseTime int64 `json:"latestLedgerCloseTime,string"`
	// LatestLedger is the oldest ledger stored in Soroban-RPC.
	OldestLedger uint32 `json:"oldestLedger"`
	// LatestLedgerCloseTime is the unix timestamp of when the oldest ledger was closed.
	OldestLedgerCloseTime int64 `json:"oldestLedgerCloseTime,string"`

	// The fields below are only present if Status is not TransactionNotFound.

	// ApplicationOrder is the index of the transaction among all the transactions
	// for that ledger.
	ApplicationOrder int32 `json:"applicationOrder,omitempty"`
	// FeeBump indicates whether the transaction is a feebump transaction
	FeeBump bool `json:"feeBump,omitempty"`
	// EnvelopeXDR is the TransactionEnvelope XDR value.
	EnvelopeXDR  string          `json:"envelopeXdr,omitempty"`
	EnvelopeJSON json.RawMessage `json:"envelopeJson,omitempty"`
	// ResultXDR is the TransactionResult XDR value.
	ResultXDR  string          `json:"resultXdr,omitempty"`
	ResultJSON json.RawMessage `json:"resultJson,omitempty"`
	// ResultMetaXDR is the TransactionMeta XDR value.
	ResultMetaXDR  string          `json:"resultMetaXdr,omitempty"`
	ResultMetaJSON json.RawMessage `json:"resultMetaJson,omitempty"`

	// Ledger is the sequence of the ledger which included the transaction.
	Ledger uint32 `json:"ledger,omitempty"`
	// LedgerCloseTime is the unix timestamp of when the transaction was included in the ledger.
	LedgerCloseTime int64 `json:"createdAt,string,omitempty"`

	// DiagnosticEventsXDR is present only if Status is equal to TransactionFailed.
	// DiagnosticEventsXDR is a base64-encoded slice of xdr.DiagnosticEvent
	DiagnosticEventsXDR  []string          `json:"diagnosticEventsXdr,omitempty"`
	DiagnosticEventsJSON []json.RawMessage `json:"diagnosticEventsJson,omitempty"`
}

type GetTransactionRequest struct {
	Hash   string `json:"hash"`
	Format string `json:"xdrFormat,omitempty"`
}

func GetTransaction(
	ctx context.Context,
	log *log.Entry,
	reader db.TransactionReader,
	ledgerReader db.LedgerReader,
	request GetTransactionRequest,
) (GetTransactionResponse, error) {
	if err := IsValidFormat(request.Format); err != nil {
		return GetTransactionResponse{}, &jrpc2.Error{
			Code:    jrpc2.InvalidParams,
			Message: err.Error(),
		}
	}

	// parse hash
	if hex.DecodedLen(len(request.Hash)) != len(xdr.Hash{}) {
		return GetTransactionResponse{}, &jrpc2.Error{
			Code:    jrpc2.InvalidParams,
			Message: fmt.Sprintf("unexpected hash length (%d)", len(request.Hash)),
		}
	}

	var txHash xdr.Hash
	_, err := hex.Decode(txHash[:], []byte(request.Hash))
	if err != nil {
		return GetTransactionResponse{}, &jrpc2.Error{
			Code:    jrpc2.InvalidParams,
			Message: fmt.Sprintf("incorrect hash: %v", err),
		}
	}

	storeRange, err := ledgerReader.GetLedgerRange(ctx)
	if err != nil {
		return GetTransactionResponse{}, &jrpc2.Error{
			Code:    jrpc2.InternalError,
			Message: fmt.Sprintf("unable to get ledger range: %v", err),
		}
	}

	tx, err := reader.GetTransaction(ctx, txHash)

	response := GetTransactionResponse{
		LatestLedger:          storeRange.LastLedger.Sequence,
		LatestLedgerCloseTime: storeRange.LastLedger.CloseTime,
		OldestLedger:          storeRange.FirstLedger.Sequence,
		OldestLedgerCloseTime: storeRange.FirstLedger.CloseTime,
	}
	if errors.Is(err, db.ErrNoTransaction) {
		response.Status = TransactionStatusNotFound
		return response, nil
	} else if err != nil {
		log.WithError(err).
			WithField("hash", txHash).
			Errorf("failed to fetch transaction")
		return response, &jrpc2.Error{
			Code:    jrpc2.InternalError,
			Message: err.Error(),
		}
	}

	response.ApplicationOrder = tx.ApplicationOrder
	response.FeeBump = tx.FeeBump
	response.Ledger = tx.Ledger.Sequence
	response.LedgerCloseTime = tx.Ledger.CloseTime

	switch request.Format {
	case FormatJSON:
		result, envelope, meta, convErr := transactionToJSON(tx)
		if convErr != nil {
			return response, &jrpc2.Error{
				Code:    jrpc2.InternalError,
				Message: convErr.Error(),
			}
		}
		diagEvents, convErr := jsonifySlice(xdr.DiagnosticEvent{}, tx.Events)
		if convErr != nil {
			return response, &jrpc2.Error{
				Code:    jrpc2.InternalError,
				Message: convErr.Error(),
			}
		}

		response.ResultJSON = result
		response.EnvelopeJSON = envelope
		response.ResultMetaJSON = meta
		response.DiagnosticEventsJSON = diagEvents

	default:
		response.ResultXDR = base64.StdEncoding.EncodeToString(tx.Result)
		response.EnvelopeXDR = base64.StdEncoding.EncodeToString(tx.Envelope)
		response.ResultMetaXDR = base64.StdEncoding.EncodeToString(tx.Meta)
		response.DiagnosticEventsXDR = base64EncodeSlice(tx.Events)
	}

	response.Status = TransactionStatusFailed
	if tx.Successful {
		response.Status = TransactionStatusSuccess
	}
	return response, nil
}

// NewGetTransactionHandler returns a get transaction json rpc handler

func NewGetTransactionHandler(logger *log.Entry, getter db.TransactionReader,
	ledgerReader db.LedgerReader,
) jrpc2.Handler {
	return NewHandler(func(ctx context.Context, request GetTransactionRequest) (GetTransactionResponse, error) {
		return GetTransaction(ctx, logger, getter, ledgerReader, request)
	})
}
