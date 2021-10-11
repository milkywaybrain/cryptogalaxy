package exchange

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/milkywaybrain/cryptogalaxy/internal/config"
	"github.com/milkywaybrain/cryptogalaxy/internal/connector"
	"github.com/milkywaybrain/cryptogalaxy/internal/storage"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
)

type probit struct {
	ws                  connector.Websocket
	rest                *connector.REST
	connCfg             *config.Connection
	cfgMap              map[cfgLookupKey]cfgLookupVal
	channelIds          map[int][2]string
	ter                 *storage.Terminal
	es                  *storage.ElasticSearch
	mysql               *storage.MySQL
	influx              *storage.InfluxDB
	nats                *storage.NATS
	clickhouse          *storage.ClickHouse
	s3                  *storage.S3
	wsTerTickers        chan []storage.Ticker
	wsTerTrades         chan []storage.Trade
	wsMysqlTickers      chan []storage.Ticker
	wsMysqlTrades       chan []storage.Trade
	wsEsTickers         chan []storage.Ticker
	wsEsTrades          chan []storage.Trade
	wsInfluxTickers     chan []storage.Ticker
	wsInfluxTrades      chan []storage.Trade
	wsNatsTickers       chan []storage.Ticker
	wsNatsTrades        chan []storage.Trade
	wsClickHouseTickers chan []storage.Ticker
	wsClickHouseTrades  chan []storage.Trade
	wsS3Tickers         chan []storage.Ticker
	wsS3Trades          chan []storage.Trade
}

type wsSubProbit struct {
	Channel  string    `json:"channel"`
	Filter   [1]string `json:"args"`
	Interval int       `json:"interval"`
	MarketID string    `json:"market_id"`
	Type     string    `json:"type"`
}

type wsRespProbit struct {
	Channel       string           `json:"channel"`
	MarketID      string           `json:"market_id"`
	Status        string           `json:"status"`
	TickerData    respDataProbit   `json:"ticker"`
	TradeData     []respDataProbit `json:"recent_trades"`
	mktCommitName string
}

type restRespProbit struct {
	Data []respDataProbit `json:"data"`
}

type respDataProbit struct {
	Side        string    `json:"side"`
	Quantity    string    `json:"quantity"`
	TickerPrice string    `json:"last"`
	TradePrice  string    `json:"price"`
	Time        time.Time `json:"time"`
}

func NewProbit(appCtx context.Context, markets []config.Market, connCfg *config.Connection) error {

	// If any exchange function fails, force all the other functions to stop and return.
	probitErrGroup, ctx := errgroup.WithContext(appCtx)

	p := probit{connCfg: connCfg}

	err := p.cfgLookup(markets)
	if err != nil {
		return err
	}

	var (
		wsCount   int
		restCount int
	)

	for _, market := range markets {
		for _, info := range market.Info {
			switch info.Connector {
			case "websocket":
				if wsCount == 0 {

					err = p.connectWs(ctx)
					if err != nil {
						return err
					}

					probitErrGroup.Go(func() error {
						return p.closeWsConnOnError(ctx)
					})

					probitErrGroup.Go(func() error {
						return p.readWs(ctx)
					})

					if p.ter != nil {
						probitErrGroup.Go(func() error {
							return p.wsTickersToTerminal(ctx)
						})
						probitErrGroup.Go(func() error {
							return p.wsTradesToTerminal(ctx)
						})
					}

					if p.mysql != nil {
						probitErrGroup.Go(func() error {
							return p.wsTickersToMySQL(ctx)
						})
						probitErrGroup.Go(func() error {
							return p.wsTradesToMySQL(ctx)
						})
					}

					if p.es != nil {
						probitErrGroup.Go(func() error {
							return p.wsTickersToES(ctx)
						})
						probitErrGroup.Go(func() error {
							return p.wsTradesToES(ctx)
						})
					}

					if p.influx != nil {
						probitErrGroup.Go(func() error {
							return p.wsTickersToInflux(ctx)
						})
						probitErrGroup.Go(func() error {
							return p.wsTradesToInflux(ctx)
						})
					}

					if p.nats != nil {
						probitErrGroup.Go(func() error {
							return p.wsTickersToNats(ctx)
						})
						probitErrGroup.Go(func() error {
							return p.wsTradesToNats(ctx)
						})
					}

					if p.clickhouse != nil {
						probitErrGroup.Go(func() error {
							return p.wsTickersToClickHouse(ctx)
						})
						probitErrGroup.Go(func() error {
							return p.wsTradesToClickHouse(ctx)
						})
					}

					if p.s3 != nil {
						probitErrGroup.Go(func() error {
							return p.wsTickersToS3(ctx)
						})
						probitErrGroup.Go(func() error {
							return p.wsTradesToS3(ctx)
						})
					}
				}

				err = p.subWsChannel(market.ID, info.Channel)
				if err != nil {
					return err
				}
				wsCount++
			case "rest":
				if restCount == 0 {
					err = p.connectRest()
					if err != nil {
						return err
					}
				}

				var mktCommitName string
				if market.CommitName != "" {
					mktCommitName = market.CommitName
				} else {
					mktCommitName = market.ID
				}
				mktID := market.ID
				channel := info.Channel
				restPingIntSec := info.RESTPingIntSec
				probitErrGroup.Go(func() error {
					return p.processREST(ctx, mktID, mktCommitName, channel, restPingIntSec)
				})

				restCount++
			}
		}
	}

	err = probitErrGroup.Wait()
	if err != nil {
		return err
	}
	return nil
}

func (p *probit) cfgLookup(markets []config.Market) error {

	// Configurations flat map is prepared for easy lookup later in the app.
	p.cfgMap = make(map[cfgLookupKey]cfgLookupVal)
	p.channelIds = make(map[int][2]string)
	for _, market := range markets {
		var mktCommitName string
		if market.CommitName != "" {
			mktCommitName = market.CommitName
		} else {
			mktCommitName = market.ID
		}
		for _, info := range market.Info {
			key := cfgLookupKey{market: market.ID, channel: info.Channel}
			val := cfgLookupVal{}
			val.connector = info.Connector
			val.wsConsiderIntSec = info.WsConsiderIntSec
			for _, str := range info.Storages {
				switch str {
				case "terminal":
					val.terStr = true
					if p.ter == nil {
						p.ter = storage.GetTerminal()
						p.wsTerTickers = make(chan []storage.Ticker, 1)
						p.wsTerTrades = make(chan []storage.Trade, 1)
					}
				case "mysql":
					val.mysqlStr = true
					if p.mysql == nil {
						p.mysql = storage.GetMySQL()
						p.wsMysqlTickers = make(chan []storage.Ticker, 1)
						p.wsMysqlTrades = make(chan []storage.Trade, 1)
					}
				case "elastic_search":
					val.esStr = true
					if p.es == nil {
						p.es = storage.GetElasticSearch()
						p.wsEsTickers = make(chan []storage.Ticker, 1)
						p.wsEsTrades = make(chan []storage.Trade, 1)
					}
				case "influxdb":
					val.influxStr = true
					if p.influx == nil {
						p.influx = storage.GetInfluxDB()
						p.wsInfluxTickers = make(chan []storage.Ticker, 1)
						p.wsInfluxTrades = make(chan []storage.Trade, 1)
					}
				case "nats":
					val.natsStr = true
					if p.nats == nil {
						p.nats = storage.GetNATS()
						p.wsNatsTickers = make(chan []storage.Ticker, 1)
						p.wsNatsTrades = make(chan []storage.Trade, 1)
					}
				case "clickhouse":
					val.clickHouseStr = true
					if p.clickhouse == nil {
						p.clickhouse = storage.GetClickHouse()
						p.wsClickHouseTickers = make(chan []storage.Ticker, 1)
						p.wsClickHouseTrades = make(chan []storage.Trade, 1)
					}
				case "s3":
					val.s3Str = true
					if p.s3 == nil {
						p.s3 = storage.GetS3()
						p.wsS3Tickers = make(chan []storage.Ticker, 1)
						p.wsS3Trades = make(chan []storage.Trade, 1)
					}
				}
			}
			val.mktCommitName = mktCommitName
			p.cfgMap[key] = val
		}
	}
	return nil
}

func (p *probit) connectWs(ctx context.Context) error {
	ws, err := connector.NewWebsocket(ctx, &p.connCfg.WS, config.ProbitWebsocketURL)
	if err != nil {
		if !errors.Is(err, ctx.Err()) {
			logErrStack(err)
		}
		return err
	}
	p.ws = ws
	log.Info().Str("exchange", "probit").Msg("websocket connected")
	return nil
}

// closeWsConnOnError closes websocket connection if there is any error in app context.
// This will unblock all read and writes on websocket.
func (p *probit) closeWsConnOnError(ctx context.Context) error {
	<-ctx.Done()
	err := p.ws.Conn.Close()
	if err != nil {
		return err
	}
	return ctx.Err()
}

// subWsChannel sends channel subscription requests to the websocket server.
func (p *probit) subWsChannel(market string, channel string) error {
	if channel == "trade" {
		channel = "recent_trades"
	}
	sub := wsSubProbit{
		Channel:  "marketdata",
		Filter:   [1]string{channel},
		Interval: 100,
		MarketID: market,
		Type:     "subscribe",
	}
	frame, err := jsoniter.Marshal(sub)
	if err != nil {
		logErrStack(err)
		return err
	}
	err = p.ws.Write(frame)
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			err = errors.New("context canceled")
		} else {
			logErrStack(err)
		}
		return err
	}
	return nil
}

// readWs reads ticker / trade data from websocket channels.
func (p *probit) readWs(ctx context.Context) error {

	// To avoid data race, creating a new local lookup map.
	cfgLookup := make(map[cfgLookupKey]cfgLookupVal, len(p.cfgMap))
	for k, v := range p.cfgMap {
		cfgLookup[k] = v
	}

	// See influxTimeVal struct doc for details.
	itv := influxTimeVal{}
	if p.influx != nil {
		itv.TickerMap = make(map[string]int64)
		itv.TradeMap = make(map[string]int64)
	}

	cd := commitData{
		terTickers:        make([]storage.Ticker, 0, p.connCfg.Terminal.TickerCommitBuf),
		terTrades:         make([]storage.Trade, 0, p.connCfg.Terminal.TradeCommitBuf),
		mysqlTickers:      make([]storage.Ticker, 0, p.connCfg.MySQL.TickerCommitBuf),
		mysqlTrades:       make([]storage.Trade, 0, p.connCfg.MySQL.TradeCommitBuf),
		esTickers:         make([]storage.Ticker, 0, p.connCfg.ES.TickerCommitBuf),
		esTrades:          make([]storage.Trade, 0, p.connCfg.ES.TradeCommitBuf),
		influxTickers:     make([]storage.Ticker, 0, p.connCfg.InfluxDB.TickerCommitBuf),
		influxTrades:      make([]storage.Trade, 0, p.connCfg.InfluxDB.TradeCommitBuf),
		natsTickers:       make([]storage.Ticker, 0, p.connCfg.NATS.TickerCommitBuf),
		natsTrades:        make([]storage.Trade, 0, p.connCfg.NATS.TradeCommitBuf),
		clickHouseTickers: make([]storage.Ticker, 0, p.connCfg.ClickHouse.TickerCommitBuf),
		clickHouseTrades:  make([]storage.Trade, 0, p.connCfg.ClickHouse.TradeCommitBuf),
		s3Tickers:         make([]storage.Ticker, 0, p.connCfg.S3.TickerCommitBuf),
		s3Trades:          make([]storage.Trade, 0, p.connCfg.S3.TradeCommitBuf),
	}

	log.Debug().Str("exchange", "probit").Str("func", "readWs").Msg("unlike other exchanges probit does not send channel subscribed success message")

	for {
		select {
		default:
			frame, err := p.ws.Read()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					err = errors.New("context canceled")
				} else {
					if err == io.EOF {
						err = errors.Wrap(err, "connection close by exchange server")
					}
					logErrStack(err)
				}
				return err
			}
			if len(frame) == 0 {
				continue
			}

			wr := wsRespProbit{}
			err = jsoniter.Unmarshal(frame, &wr)
			if err != nil {
				logErrStack(err)
				return err
			}

			if wr.Status != "ok" {
				log.Error().Str("exchange", "probit").Str("func", "readWs").Str("msg", wr.Status).Msg("")
				return errors.New("probit websocket error")
			}

			// Consider frame only in configured interval, otherwise ignore it.

			// Ticker.
			if wr.TickerData.TickerPrice != "" {
				wr.Channel = "ticker"
				key := cfgLookupKey{market: wr.MarketID, channel: "ticker"}
				val := cfgLookup[key]
				if val.wsConsiderIntSec == 0 || time.Since(val.wsLastUpdated).Seconds() >= float64(val.wsConsiderIntSec) {
					val.wsLastUpdated = time.Now()
					wr.mktCommitName = val.mktCommitName
					cfgLookup[key] = val
				} else {
					continue
				}

				err := p.processWs(ctx, &wr, &cd, &itv)
				if err != nil {
					return err
				}
			}

			// Trade.
			if len(wr.TradeData) > 0 {
				wr.Channel = "trade"
				key := cfgLookupKey{market: wr.MarketID, channel: "trade"}
				val := cfgLookup[key]
				if val.wsConsiderIntSec == 0 || time.Since(val.wsLastUpdated).Seconds() >= float64(val.wsConsiderIntSec) {
					val.wsLastUpdated = time.Now()
					wr.mktCommitName = val.mktCommitName
					cfgLookup[key] = val
				} else {
					continue
				}

				err := p.processWs(ctx, &wr, &cd, &itv)
				if err != nil {
					return err
				}
			}

		// Return, if there is any error from another function or exchange.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// processWs receives ticker / trade data,
// transforms it to a common ticker / trade store format,
// buffers the same in memory and
// then sends it to different storage systems for commit through go channels.
func (p *probit) processWs(ctx context.Context, wr *wsRespProbit, cd *commitData, itv *influxTimeVal) error {
	switch wr.Channel {
	case "ticker":
		ticker := storage.Ticker{}
		ticker.Exchange = "probit"
		ticker.MktID = wr.MarketID
		ticker.MktCommitName = wr.mktCommitName

		price, err := strconv.ParseFloat(wr.TickerData.TickerPrice, 64)
		if err != nil {
			logErrStack(err)
			return err
		}
		ticker.Price = price

		ticker.Timestamp = wr.TickerData.Time

		key := cfgLookupKey{market: ticker.MktID, channel: "ticker"}
		val := p.cfgMap[key]
		if val.terStr {
			cd.terTickersCount++
			cd.terTickers = append(cd.terTickers, ticker)
			if cd.terTickersCount == p.connCfg.Terminal.TickerCommitBuf {
				select {
				case p.wsTerTickers <- cd.terTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.terTickersCount = 0
				cd.terTickers = nil
			}
		}
		if val.mysqlStr {
			cd.mysqlTickersCount++
			cd.mysqlTickers = append(cd.mysqlTickers, ticker)
			if cd.mysqlTickersCount == p.connCfg.MySQL.TickerCommitBuf {
				select {
				case p.wsMysqlTickers <- cd.mysqlTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.mysqlTickersCount = 0
				cd.mysqlTickers = nil
			}
		}
		if val.esStr {
			cd.esTickersCount++
			cd.esTickers = append(cd.esTickers, ticker)
			if cd.esTickersCount == p.connCfg.ES.TickerCommitBuf {
				select {
				case p.wsEsTickers <- cd.esTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.esTickersCount = 0
				cd.esTickers = nil
			}
		}
		if val.influxStr {
			val := itv.TickerMap[ticker.MktCommitName]
			if val == 0 || val == 999999 {
				val = 1
			} else {
				val++
			}
			itv.TickerMap[ticker.MktCommitName] = val
			ticker.InfluxVal = val

			cd.influxTickersCount++
			cd.influxTickers = append(cd.influxTickers, ticker)
			if cd.influxTickersCount == p.connCfg.InfluxDB.TickerCommitBuf {
				select {
				case p.wsInfluxTickers <- cd.influxTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.influxTickersCount = 0
				cd.influxTickers = nil
			}
		}
		if val.natsStr {
			cd.natsTickersCount++
			cd.natsTickers = append(cd.natsTickers, ticker)
			if cd.natsTickersCount == p.connCfg.NATS.TickerCommitBuf {
				select {
				case p.wsNatsTickers <- cd.natsTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.natsTickersCount = 0
				cd.natsTickers = nil
			}
		}
		if val.clickHouseStr {
			cd.clickHouseTickersCount++
			cd.clickHouseTickers = append(cd.clickHouseTickers, ticker)
			if cd.clickHouseTickersCount == p.connCfg.ClickHouse.TickerCommitBuf {
				select {
				case p.wsClickHouseTickers <- cd.clickHouseTickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.clickHouseTickersCount = 0
				cd.clickHouseTickers = nil
			}
		}
		if val.s3Str {
			cd.s3TickersCount++
			cd.s3Tickers = append(cd.s3Tickers, ticker)
			if cd.s3TickersCount == p.connCfg.S3.TickerCommitBuf {
				select {
				case p.wsS3Tickers <- cd.s3Tickers:
				case <-ctx.Done():
					return ctx.Err()
				}
				cd.s3TickersCount = 0
				cd.s3Tickers = nil
			}
		}
	case "trade":
		for _, data := range wr.TradeData {
			trade := storage.Trade{}
			trade.Exchange = "probit"
			trade.MktID = wr.MarketID
			trade.MktCommitName = wr.mktCommitName
			trade.Side = data.Side

			size, err := strconv.ParseFloat(data.Quantity, 64)
			if err != nil {
				logErrStack(err)
				return err
			}
			trade.Size = size

			price, err := strconv.ParseFloat(data.TradePrice, 64)
			if err != nil {
				logErrStack(err)
				return err
			}
			trade.Price = price

			trade.Timestamp = data.Time

			key := cfgLookupKey{market: trade.MktID, channel: "trade"}
			val := p.cfgMap[key]
			if val.terStr {
				cd.terTradesCount++
				cd.terTrades = append(cd.terTrades, trade)
				if cd.terTradesCount == p.connCfg.Terminal.TradeCommitBuf {
					select {
					case p.wsTerTrades <- cd.terTrades:
					case <-ctx.Done():
						return ctx.Err()
					}
					cd.terTradesCount = 0
					cd.terTrades = nil
				}
			}
			if val.mysqlStr {
				cd.mysqlTradesCount++
				cd.mysqlTrades = append(cd.mysqlTrades, trade)
				if cd.mysqlTradesCount == p.connCfg.MySQL.TradeCommitBuf {
					select {
					case p.wsMysqlTrades <- cd.mysqlTrades:
					case <-ctx.Done():
						return ctx.Err()
					}
					cd.mysqlTradesCount = 0
					cd.mysqlTrades = nil
				}
			}
			if val.esStr {
				cd.esTradesCount++
				cd.esTrades = append(cd.esTrades, trade)
				if cd.esTradesCount == p.connCfg.ES.TradeCommitBuf {
					select {
					case p.wsEsTrades <- cd.esTrades:
					case <-ctx.Done():
						return ctx.Err()
					}
					cd.esTradesCount = 0
					cd.esTrades = nil
				}
			}
			if val.influxStr {
				val := itv.TradeMap[trade.MktCommitName]
				if val == 0 || val == 999999 {
					val = 1
				} else {
					val++
				}
				itv.TradeMap[trade.MktCommitName] = val
				trade.InfluxVal = val

				cd.influxTradesCount++
				cd.influxTrades = append(cd.influxTrades, trade)
				if cd.influxTradesCount == p.connCfg.InfluxDB.TradeCommitBuf {
					select {
					case p.wsInfluxTrades <- cd.influxTrades:
					case <-ctx.Done():
						return ctx.Err()
					}
					cd.influxTradesCount = 0
					cd.influxTrades = nil
				}
			}
			if val.natsStr {
				cd.natsTradesCount++
				cd.natsTrades = append(cd.natsTrades, trade)
				if cd.natsTradesCount == p.connCfg.NATS.TradeCommitBuf {
					select {
					case p.wsNatsTrades <- cd.natsTrades:
					case <-ctx.Done():
						return ctx.Err()
					}
					cd.natsTradesCount = 0
					cd.natsTrades = nil
				}
			}
			if val.clickHouseStr {
				cd.clickHouseTradesCount++
				cd.clickHouseTrades = append(cd.clickHouseTrades, trade)
				if cd.clickHouseTradesCount == p.connCfg.ClickHouse.TradeCommitBuf {
					select {
					case p.wsClickHouseTrades <- cd.clickHouseTrades:
					case <-ctx.Done():
						return ctx.Err()
					}
					cd.clickHouseTradesCount = 0
					cd.clickHouseTrades = nil
				}
			}
			if val.s3Str {
				cd.s3TradesCount++
				cd.s3Trades = append(cd.s3Trades, trade)
				if cd.s3TradesCount == p.connCfg.S3.TradeCommitBuf {
					select {
					case p.wsS3Trades <- cd.s3Trades:
					case <-ctx.Done():
						return ctx.Err()
					}
					cd.s3TradesCount = 0
					cd.s3Trades = nil
				}
			}
		}
	}
	return nil
}

func (p *probit) wsTickersToTerminal(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsTerTickers:
			p.ter.CommitTickers(data)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *probit) wsTradesToTerminal(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsTerTrades:
			p.ter.CommitTrades(data)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *probit) wsTickersToMySQL(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsMysqlTickers:
			err := p.mysql.CommitTickers(ctx, data)
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

func (p *probit) wsTradesToMySQL(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsMysqlTrades:
			err := p.mysql.CommitTrades(ctx, data)
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

func (p *probit) wsTickersToES(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsEsTickers:
			err := p.es.CommitTickers(ctx, data)
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

func (p *probit) wsTradesToES(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsEsTrades:
			err := p.es.CommitTrades(ctx, data)
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

func (p *probit) wsTickersToInflux(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsInfluxTickers:
			err := p.influx.CommitTickers(ctx, data)
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

func (p *probit) wsTradesToInflux(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsInfluxTrades:
			err := p.influx.CommitTrades(ctx, data)
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

func (p *probit) wsTickersToNats(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsNatsTickers:
			err := p.nats.CommitTickers(data)
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *probit) wsTradesToNats(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsNatsTrades:
			err := p.nats.CommitTrades(data)
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *probit) wsTickersToClickHouse(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsClickHouseTickers:
			err := p.clickhouse.CommitTickers(ctx, data)
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

func (p *probit) wsTradesToClickHouse(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsClickHouseTrades:
			err := p.clickhouse.CommitTrades(ctx, data)
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

func (p *probit) wsTickersToS3(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsS3Tickers:
			err := p.s3.CommitTickers(ctx, data)
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

func (p *probit) wsTradesToS3(ctx context.Context) error {
	for {
		select {
		case data := <-p.wsS3Trades:
			err := p.s3.CommitTrades(ctx, data)
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

func (p *probit) connectRest() error {
	rest, err := connector.GetREST()
	if err != nil {
		logErrStack(err)
		return err
	}
	p.rest = rest
	log.Info().Str("exchange", "probit").Msg("REST connection setup is done")
	return nil
}

// processREST queries exchange for ticker / trade data through REST API in configured intervals,
// transforms it to a common ticker / trade store format,
// buffers the same in memory and
// then sends it to different storage systems for commit through go channels.
func (p *probit) processREST(ctx context.Context, mktID string, mktCommitName string, channel string, interval int) error {
	var (
		req *http.Request
		q   url.Values
		err error

		// See influxTimeVal (exchange.go) struct doc for details.
		influxTickerTime int64
		influxTradeTime  int64
	)

	const timeFormat = "2006-01-02T15:04:05.999Z"

	cd := commitData{
		terTickers:        make([]storage.Ticker, 0, p.connCfg.Terminal.TickerCommitBuf),
		terTrades:         make([]storage.Trade, 0, p.connCfg.Terminal.TradeCommitBuf),
		mysqlTickers:      make([]storage.Ticker, 0, p.connCfg.MySQL.TickerCommitBuf),
		mysqlTrades:       make([]storage.Trade, 0, p.connCfg.MySQL.TradeCommitBuf),
		esTickers:         make([]storage.Ticker, 0, p.connCfg.ES.TickerCommitBuf),
		esTrades:          make([]storage.Trade, 0, p.connCfg.ES.TradeCommitBuf),
		influxTickers:     make([]storage.Ticker, 0, p.connCfg.InfluxDB.TickerCommitBuf),
		influxTrades:      make([]storage.Trade, 0, p.connCfg.InfluxDB.TradeCommitBuf),
		natsTickers:       make([]storage.Ticker, 0, p.connCfg.NATS.TickerCommitBuf),
		natsTrades:        make([]storage.Trade, 0, p.connCfg.NATS.TradeCommitBuf),
		clickHouseTickers: make([]storage.Ticker, 0, p.connCfg.ClickHouse.TickerCommitBuf),
		clickHouseTrades:  make([]storage.Trade, 0, p.connCfg.ClickHouse.TradeCommitBuf),
		s3Tickers:         make([]storage.Ticker, 0, p.connCfg.S3.TickerCommitBuf),
		s3Trades:          make([]storage.Trade, 0, p.connCfg.S3.TradeCommitBuf),
	}

	switch channel {
	case "ticker":
		req, err = p.rest.Request(ctx, "GET", config.ProbitRESTBaseURL+"ticker")
		if err != nil {
			if !errors.Is(err, ctx.Err()) {
				logErrStack(err)
			}
			return err
		}
		q = req.URL.Query()
		q.Add("market_ids", mktID)
	case "trade":
		req, err = p.rest.Request(ctx, "GET", config.ProbitRESTBaseURL+"trade")
		if err != nil {
			if !errors.Is(err, ctx.Err()) {
				logErrStack(err)
			}
			return err
		}
		q = req.URL.Query()
		q.Add("market_id", mktID)

		// Querying for 100 trades.
		// If the configured interval gap is big, then maybe it will not return all the trades
		// and if the gap is too small, maybe it will return duplicate ones.
		// Better to use websocket.
		q.Add("limit", strconv.Itoa(100))
	}

	tick := time.NewTicker(time.Duration(interval) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:

			switch channel {
			case "ticker":
				req.URL.RawQuery = q.Encode()
				resp, err := p.rest.Do(req)
				if err != nil {
					if !errors.Is(err, ctx.Err()) {
						logErrStack(err)
					}
					return err
				}

				rr := restRespProbit{}
				if err = jsoniter.NewDecoder(resp.Body).Decode(&rr); err != nil {
					logErrStack(err)
					resp.Body.Close()
					return err
				}
				resp.Body.Close()

				r := rr.Data[0]

				price, err := strconv.ParseFloat(r.TickerPrice, 64)
				if err != nil {
					logErrStack(err)
					return err
				}

				ticker := storage.Ticker{
					Exchange:      "probit",
					MktID:         mktID,
					MktCommitName: mktCommitName,
					Price:         price,
					Timestamp:     time.Now().UTC(),
				}

				key := cfgLookupKey{market: ticker.MktID, channel: "ticker"}
				val := p.cfgMap[key]
				if val.terStr {
					cd.terTickersCount++
					cd.terTickers = append(cd.terTickers, ticker)
					if cd.terTickersCount == p.connCfg.Terminal.TickerCommitBuf {
						p.ter.CommitTickers(cd.terTickers)
						cd.terTickersCount = 0
						cd.terTickers = nil
					}
				}
				if val.mysqlStr {
					cd.mysqlTickersCount++
					cd.mysqlTickers = append(cd.mysqlTickers, ticker)
					if cd.mysqlTickersCount == p.connCfg.MySQL.TickerCommitBuf {
						err := p.mysql.CommitTickers(ctx, cd.mysqlTickers)
						if err != nil {
							if !errors.Is(err, ctx.Err()) {
								logErrStack(err)
							}
							return err
						}
						cd.mysqlTickersCount = 0
						cd.mysqlTickers = nil
					}
				}
				if val.esStr {
					cd.esTickersCount++
					cd.esTickers = append(cd.esTickers, ticker)
					if cd.esTickersCount == p.connCfg.ES.TickerCommitBuf {
						err := p.es.CommitTickers(ctx, cd.esTickers)
						if err != nil {
							if !errors.Is(err, ctx.Err()) {
								logErrStack(err)
							}
							return err
						}
						cd.esTickersCount = 0
						cd.esTickers = nil
					}
				}
				if val.influxStr {
					if influxTickerTime == 0 || influxTickerTime == 999999 {
						influxTickerTime = 1
					} else {
						influxTickerTime++
					}
					ticker.InfluxVal = influxTickerTime

					cd.influxTickersCount++
					cd.influxTickers = append(cd.influxTickers, ticker)
					if cd.influxTickersCount == p.connCfg.InfluxDB.TickerCommitBuf {
						err := p.influx.CommitTickers(ctx, cd.influxTickers)
						if err != nil {
							if !errors.Is(err, ctx.Err()) {
								logErrStack(err)
							}
							return err
						}
						cd.influxTickersCount = 0
						cd.influxTickers = nil
					}
				}
				if val.natsStr {
					cd.natsTickersCount++
					cd.natsTickers = append(cd.natsTickers, ticker)
					if cd.natsTickersCount == p.connCfg.NATS.TickerCommitBuf {
						err := p.nats.CommitTickers(cd.natsTickers)
						if err != nil {
							return err
						}
						cd.natsTickersCount = 0
						cd.natsTickers = nil
					}
				}
				if val.clickHouseStr {
					cd.clickHouseTickersCount++
					cd.clickHouseTickers = append(cd.clickHouseTickers, ticker)
					if cd.clickHouseTickersCount == p.connCfg.ClickHouse.TickerCommitBuf {
						err := p.clickhouse.CommitTickers(ctx, cd.clickHouseTickers)
						if err != nil {
							return err
						}
						cd.clickHouseTickersCount = 0
						cd.clickHouseTickers = nil
					}
				}
				if val.s3Str {
					cd.s3TickersCount++
					cd.s3Tickers = append(cd.s3Tickers, ticker)
					if cd.s3TickersCount == p.connCfg.S3.TickerCommitBuf {
						err := p.s3.CommitTickers(ctx, cd.s3Tickers)
						if err != nil {
							return err
						}
						cd.s3TickersCount = 0
						cd.s3Tickers = nil
					}
				}
			case "trade":

				// Really, better to use websocket. Start and end time for getting trade data is constructed randomly!
				q.Del("start_time")
				q.Del("end_time")
				currentTime := time.Now().UTC()
				oldTime := currentTime.Add(-15 * time.Minute)
				q.Add("start_time", oldTime.Format(timeFormat))
				q.Add("end_time", currentTime.Format(timeFormat))

				req.URL.RawQuery = q.Encode()
				resp, err := p.rest.Do(req)
				if err != nil {
					if !errors.Is(err, ctx.Err()) {
						logErrStack(err)
					}
					return err
				}

				rr := restRespProbit{}
				if err := jsoniter.NewDecoder(resp.Body).Decode(&rr); err != nil {
					logErrStack(err)
					resp.Body.Close()
					return err
				}
				resp.Body.Close()

				for i := range rr.Data {
					r := rr.Data[i]

					size, err := strconv.ParseFloat(r.Quantity, 64)
					if err != nil {
						logErrStack(err)
						return err
					}

					price, err := strconv.ParseFloat(r.TradePrice, 64)
					if err != nil {
						logErrStack(err)
						return err
					}

					trade := storage.Trade{
						Exchange:      "probit",
						MktID:         mktID,
						MktCommitName: mktCommitName,
						Side:          r.Side,
						Size:          size,
						Price:         price,
						Timestamp:     r.Time,
					}

					key := cfgLookupKey{market: trade.MktID, channel: "trade"}
					val := p.cfgMap[key]
					if val.terStr {
						cd.terTradesCount++
						cd.terTrades = append(cd.terTrades, trade)
						if cd.terTradesCount == p.connCfg.Terminal.TradeCommitBuf {
							p.ter.CommitTrades(cd.terTrades)
							cd.terTradesCount = 0
							cd.terTrades = nil
						}
					}
					if val.mysqlStr {
						cd.mysqlTradesCount++
						cd.mysqlTrades = append(cd.mysqlTrades, trade)
						if cd.mysqlTradesCount == p.connCfg.MySQL.TradeCommitBuf {
							err := p.mysql.CommitTrades(ctx, cd.mysqlTrades)
							if err != nil {
								if !errors.Is(err, ctx.Err()) {
									logErrStack(err)
								}
								return err
							}
							cd.mysqlTradesCount = 0
							cd.mysqlTrades = nil
						}
					}
					if val.esStr {
						cd.esTradesCount++
						cd.esTrades = append(cd.esTrades, trade)
						if cd.esTradesCount == p.connCfg.ES.TradeCommitBuf {
							err := p.es.CommitTrades(ctx, cd.esTrades)
							if err != nil {
								if !errors.Is(err, ctx.Err()) {
									logErrStack(err)
								}
								return err
							}
							cd.esTradesCount = 0
							cd.esTrades = nil
						}
					}
					if val.influxStr {
						if influxTradeTime == 0 || influxTradeTime == 999999 {
							influxTradeTime = 1
						} else {
							influxTradeTime++
						}
						trade.InfluxVal = influxTradeTime

						cd.influxTradesCount++
						cd.influxTrades = append(cd.influxTrades, trade)
						if cd.influxTradesCount == p.connCfg.InfluxDB.TradeCommitBuf {
							err := p.influx.CommitTrades(ctx, cd.influxTrades)
							if err != nil {
								if !errors.Is(err, ctx.Err()) {
									logErrStack(err)
								}
								return err
							}
							cd.influxTradesCount = 0
							cd.influxTrades = nil
						}
					}
					if val.natsStr {
						cd.natsTradesCount++
						cd.natsTrades = append(cd.natsTrades, trade)
						if cd.natsTradesCount == p.connCfg.NATS.TradeCommitBuf {
							err := p.nats.CommitTrades(cd.natsTrades)
							if err != nil {
								return err
							}
							cd.natsTradesCount = 0
							cd.natsTrades = nil
						}
					}
					if val.clickHouseStr {
						cd.clickHouseTradesCount++
						cd.clickHouseTrades = append(cd.clickHouseTrades, trade)
						if cd.clickHouseTradesCount == p.connCfg.ClickHouse.TradeCommitBuf {
							err := p.clickhouse.CommitTrades(ctx, cd.clickHouseTrades)
							if err != nil {
								if !errors.Is(err, ctx.Err()) {
									logErrStack(err)
								}
								return err
							}
							cd.clickHouseTradesCount = 0
							cd.clickHouseTrades = nil
						}
					}
					if val.s3Str {
						cd.s3TradesCount++
						cd.s3Trades = append(cd.s3Trades, trade)
						if cd.s3TradesCount == p.connCfg.S3.TradeCommitBuf {
							err := p.s3.CommitTrades(ctx, cd.s3Trades)
							if err != nil {
								if !errors.Is(err, ctx.Err()) {
									logErrStack(err)
								}
								return err
							}
							cd.s3TradesCount = 0
							cd.s3Trades = nil
						}
					}
				}
			}

		// Return, if there is any error from another function or exchange.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
