// Copyright 2018 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Scrape heartbeat data.

package collector

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// heartbeat is the Metric subsystem we use.
	heartbeat = "heartbeat"

	// heartbeatQuery fetches:
	//  1. heartbeat row timestamp
	//  2. current server timestamp at the same moment
	//  3. server_id that wrote the heartbeat row
	//
	// %s placeholders are:
	//  - current time expression (NOW(6) / UTC_TIMESTAMP(6))
	//  - database name
	//  - table name
	heartbeatQuery = "SELECT UNIX_TIMESTAMP(ts), UNIX_TIMESTAMP(%s), server_id FROM %s.%s"
)

var (
	collectHeartbeatDatabase = kingpin.Flag(
		"collect.heartbeat.database",
		"Database from where to collect heartbeat data",
	).Default("heartbeat").String()
	collectHeartbeatTable = kingpin.Flag(
		"collect.heartbeat.table",
		"Table from where to collect heartbeat data",
	).Default("heartbeat").String()
	collectHeartbeatUtc = kingpin.Flag(
		"collect.heartbeat.utc",
		"Use UTC for timestamps of the current server (`pt-heartbeat` is called with `--utc`)",
	).Bool()
)

// Metric descriptors.
var (
	HeartbeatServerTimestampDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, heartbeat, "server_timestamp_seconds"),
		"Timestamp stored in the heartbeat table for a given server_id.",
		[]string{"server_id"},
		nil,
	)

	HeartbeatServerIsLatestDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, heartbeat, "server_is_latest"),
		"Whether this server_id owns the freshest heartbeat row.",
		[]string{"server_id"},
		nil,
	)

	HeartbeatLagDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, heartbeat, "lag_seconds"),
		"Replication lag based on the freshest heartbeat row.",
		nil,
		nil,
	)

	HeartbeatLatestServerDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, heartbeat, "latest_server_info"),
		"Info metric for the server_id with the freshest heartbeat row.",
		[]string{"server_id"},
		nil,
	)
)

// ScrapeHeartbeat scrapes from the heartbeat table.
// This is mainly targeting pt-heartbeat, but will work with any heartbeat
// implementation that writes to a table with two columns:
//
// CREATE TABLE heartbeat (
//
//	server_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
//	ts DATETIME(6) NOT NULL
//
// );
type ScrapeHeartbeat struct{}

// Name of the Scraper. Should be unique.
func (ScrapeHeartbeat) Name() string {
	return "heartbeat"
}

// Help describes the role of the Scraper.
func (ScrapeHeartbeat) Help() string {
	return "Collect from heartbeat"
}

// Version of MySQL from which scraper is available.
func (ScrapeHeartbeat) Version() float64 {
	return 5.1
}

// nowExpr returns a current timestamp expression.
func nowExpr() string {
	if *collectHeartbeatUtc {
		return "UTC_TIMESTAMP(6)"
	}
	return "NOW(6)"
}

type heartbeatRow struct {
	ServerID string
	TS       float64
	Now      float64
}

// Scrape collects data from a database connection and sends it over a channel as prometheus metric.
func (ScrapeHeartbeat) Scrape(ctx context.Context, instance *instance, ch chan<- prometheus.Metric, logger *slog.Logger) error {
	_ = logger

	db := instance.getDB()

	query := fmt.Sprintf(
		heartbeatQuery,
		nowExpr(),
		quoteIdentifier(*collectHeartbeatDatabase),
		quoteIdentifier(*collectHeartbeatTable),
	)

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	var (
		allRows []heartbeatRow
		latest  *heartbeatRow
	)

	for rows.Next() {
		var (
			ts       float64
			now      float64
			serverID uint64
		)

		if err := rows.Scan(&ts, &now, &serverID); err != nil {
			return err
		}

		row := heartbeatRow{
			ServerID: strconv.FormatUint(serverID, 10),
			TS:       ts,
			Now:      now,
		}

		allRows = append(allRows, row)

		// Deterministic winner:
		// 1. fresher TS wins
		// 2. on equal TS, bigger server_id wins
		if latest == nil ||
			row.TS > latest.TS ||
			(row.TS == latest.TS && row.ServerID > latest.ServerID) {
			copyRow := row
			latest = &copyRow
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}

	if latest == nil {
		return nil
	}

	for _, row := range allRows {
		ch <- prometheus.MustNewConstMetric(HeartbeatServerTimestampDesc, prometheus.GaugeValue, row.TS, row.ServerID)

		isLatest := 0.0
		if row.ServerID == latest.ServerID {
			isLatest = 1.0
		}

		ch <- prometheus.MustNewConstMetric(HeartbeatServerIsLatestDesc, prometheus.GaugeValue, isLatest, row.ServerID)
	}

	lag := latest.Now - latest.TS
	if lag < 0 {
		lag = 0
	}

	ch <- prometheus.MustNewConstMetric(HeartbeatLagDesc, prometheus.GaugeValue, lag)
	ch <- prometheus.MustNewConstMetric(HeartbeatLatestServerDesc, prometheus.GaugeValue, 1, latest.ServerID)

	return nil
}

func quoteIdentifier(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// check interface
var _ Scraper = ScrapeHeartbeat{}
