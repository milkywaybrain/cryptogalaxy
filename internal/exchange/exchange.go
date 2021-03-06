package exchange

import (
	"context"
	"time"

	"github.com/milkywaybrain/cryptogalaxy/internal/storage"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

// cfgLookupKey is a key in the config lookup map.
type cfgLookupKey struct {
	market  string
	channel string
}

// cfgLookupVal is a value in the config lookup map.
type cfgLookupVal struct {
	connector        string
	wsConsiderIntSec int
	wsLastUpdated    time.Time
	terStr           bool
	mysqlStr         bool
	esStr            bool
	influxStr        bool
	natsStr          bool
	clickHouseStr    bool
	s3Str            bool
	id               int
	mktCommitName    string
}

type commitData struct {
	terTickersCount        int
	terTradesCount         int
	mysqlTickersCount      int
	mysqlTradesCount       int
	esTickersCount         int
	esTradesCount          int
	influxTickersCount     int
	influxTradesCount      int
	natsTickersCount       int
	natsTradesCount        int
	clickHouseTickersCount int
	clickHouseTradesCount  int
	s3TickersCount         int
	s3TradesCount          int
	terTickers             []storage.Ticker
	terTrades              []storage.Trade
	mysqlTickers           []storage.Ticker
	mysqlTrades            []storage.Trade
	esTickers              []storage.Ticker
	esTrades               []storage.Trade
	influxTickers          []storage.Ticker
	influxTrades           []storage.Trade
	natsTickers            []storage.Ticker
	natsTrades             []storage.Trade
	clickHouseTickers      []storage.Ticker
	clickHouseTrades       []storage.Trade
	s3Tickers              []storage.Ticker
	s3Trades               []storage.Trade
}

type influxTimeVal struct {

	// Sometime, ticker and trade data that we receive from the exchanges will have multiple records for the same timestamp.
	// This data is deleted automatically by the InfluxDB as the system identifies unique data points by
	// their measurement, tag set, and timestamp. Also we cannot add a unique id or timestamp as a new tag to the data set
	// as it may significantly affect the performance of the InfluxDB read / writes. So to solve this problem,
	// here we are adding 1 nanosecond to each timestamp entry of exchange and market combo till it reaches
	// 1 millisecond to have a unique timestamp entry for each data point. This will not change anything
	// as we are maintaining only millisecond precision ticker and trade records.
	// Of course this will break if we have more than a million trades per millisecond per market in an exchange. But we
	// are excluding that scenario.
	TickerMap map[string]int64
	TradeMap  map[string]int64
}

// WsTickersToStorage batch inserts input ticker data from websocket to specified storage.
func WsTickersToStorage(ctx context.Context, str storage.Storage, tickers <-chan []storage.Ticker) error {
	for {
		select {
		case data := <-tickers:
			err := str.CommitTickers(ctx, data)
			if err != nil {
				if !errors.Is(err, ctx.Err()) {
					logErrStack(err)
				}
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// WsTradesToStorage batch inserts input trade data from websocket to specified storage.
func WsTradesToStorage(ctx context.Context, str storage.Storage, trades <-chan []storage.Trade) error {
	for {
		select {
		case data := <-trades:
			err := str.CommitTrades(ctx, data)
			if err != nil {
				if !errors.Is(err, ctx.Err()) {
					logErrStack(err)
				}
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// logErrStack logs error with stack trace.
func logErrStack(err error) {
	log.Error().Stack().Err(errors.WithStack(err)).Msg("")
}
