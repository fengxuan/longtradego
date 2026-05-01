package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/longbridge/openapi-go/quote"
	lbprotocol "github.com/longbridge/openapi-protocol/go"
	lbclient "github.com/longbridge/openapi-protocol/go/client"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newQuoteCommand(app *appContext) *cobra.Command {
	return &cobra.Command{
		Use:     "quote [SYMBOL ...]",
		Aliases: []string{"q"},
		Short:   "Get real-time quotes for one or more symbols",
		Example: "longtradego quote AAPL.US TSLA.US 700.HK",
		RunE: func(cmd *cobra.Command, args []string) error {
			symbols := parseSymbols(args)
			if len(symbols) == 0 {
				symbols = []string{"AAPL.US"}
			}

			app.SetExecution("quote", symbols)

			result, err := runQuote(cmd.Context(), app, symbols)
			if err != nil {
				return err
			}
			app.SetResult(result)
			return nil
		},
	}
}

func runQuote(ctx context.Context, app *appContext, symbols []string) (*quoteCommandResult, error) {
	quotes, recovered, err := queryQuotesWithRecovery(ctx, app, symbols)
	if err != nil {
		return nil, err
	}
	if len(quotes) == 0 {
		return nil, fmt.Errorf("no quote returned for %v", symbols)
	}

	result := &quoteCommandResult{
		QuoteCount: len(quotes),
		Quotes:     make([]quoteResultItem, 0, len(quotes)),
		Recovered:  recovered,
	}

	for i, q := range quotes {
		item := buildQuoteResultItem(q)
		result.Quotes = append(result.Quotes, item)

		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("Symbol:      %s\n", item.Symbol)
		fmt.Printf("Last Done:   %s\n", item.LastDone)
		fmt.Printf("Prev Close:  %s\n", item.PrevClose)
		fmt.Printf("Open:        %s\n", item.Open)
		fmt.Printf("High:        %s\n", item.High)
		fmt.Printf("Low:         %s\n", item.Low)
		fmt.Printf("Volume:      %d\n", item.Volume)
		fmt.Printf("Turnover:    %s\n", item.Turnover)
		fmt.Printf("Timestamp:   %s\n", item.Timestamp)
		fmt.Printf("TradeStatus: %s\n", item.TradeStatus)
	}
	return result, nil
}

type quoteCommandResult struct {
	QuoteCount int               `json:"quote_count"`
	Quotes     []quoteResultItem `json:"quotes"`
	Recovered  bool              `json:"recovered,omitempty"`
}

type quoteResultItem struct {
	Symbol        string `json:"symbol"`
	LastDone      string `json:"last_done"`
	PrevClose     string `json:"prev_close"`
	Open          string `json:"open"`
	High          string `json:"high"`
	Low           string `json:"low"`
	Volume        int64  `json:"volume"`
	Turnover      string `json:"turnover"`
	Timestamp     string `json:"timestamp"`
	TimestampUnix int64  `json:"timestamp_unix"`
	TradeStatus   string `json:"trade_status"`
}

func buildQuoteResultItem(q *quote.SecurityQuote) quoteResultItem {
	return quoteResultItem{
		Symbol:        q.Symbol,
		LastDone:      decimalText(q.LastDone),
		PrevClose:     decimalText(q.PrevClose),
		Open:          decimalText(q.Open),
		High:          decimalText(q.High),
		Low:           decimalText(q.Low),
		Volume:        q.Volume,
		Turnover:      decimalText(q.Turnover),
		Timestamp:     timestampText(q.Timestamp),
		TimestampUnix: q.Timestamp,
		TradeStatus:   fmt.Sprintf("%v", q.TradeStatus),
	}
}

func decimalText(value *decimal.Decimal) string {
	if value == nil {
		return "-"
	}
	return value.String()
}

func timestampText(timestamp int64) string {
	if timestamp == 0 {
		return "-"
	}
	return time.Unix(timestamp, 0).Format(time.RFC3339)
}

func queryQuotesWithRecovery(ctx context.Context, app *appContext, symbols []string) ([]*quote.SecurityQuote, bool, error) {
	quoteContext, err := app.QuoteContext(ctx)
	if err != nil {
		return nil, false, err
	}

	quotes, err := quoteContext.Quote(ctx, symbols)
	if err == nil {
		return quotes, false, nil
	}
	if !isRecoverableQuoteError(err) {
		return nil, false, err
	}

	// Lazy reconnect only when a command is running and session is expired.
	_ = app.ResetQuoteContext()

	quoteContext, reinitErr := app.QuoteContext(ctx)
	if reinitErr != nil {
		return nil, true, fmt.Errorf("quote context recovery failed: %w (original: %v)", reinitErr, err)
	}

	quotes, retryErr := quoteContext.Quote(ctx, symbols)
	if retryErr != nil {
		return nil, true, retryErr
	}
	return quotes, true, nil
}

func isRecoverableQuoteError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, lbclient.ErrSessExpired) {
		return true
	}

	var protocolErr *lbprotocol.LBError
	if errors.As(err, &protocolErr) {
		if protocolErr.Status == lbprotocol.StatusUnauthenticated {
			return true
		}
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "session expired"):
		return true
	case strings.Contains(msg, "status:3"):
		return true
	case strings.Contains(msg, "unauthenticated"):
		return true
	case strings.Contains(msg, "client conn closed"):
		return true
	case strings.Contains(msg, "close by server"):
		return true
	case strings.Contains(msg, "reconnect request"):
		return true
	default:
		return false
	}
}
