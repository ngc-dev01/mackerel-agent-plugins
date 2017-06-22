package mppostgres

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
	// PostgreSQL Driver
	_ "github.com/lib/pq"
	mp "github.com/mackerelio/go-mackerel-plugin-helper"
	"github.com/mackerelio/mackerel-agent/logging"
)

var logger = logging.GetLogger("metrics.plugin.postgres")

// PostgresPlugin mackerel plugin for PostgreSQL
type PostgresPlugin struct {
	Host     string
	Port     string
	Username string
	Password string
	SSLmode  string
	Prefix   string
	Timeout  int
	Tempfile string
	Option   string
}

func fetchStatDatabase(db *sqlx.DB) (map[string]interface{}, error) {
	db = db.Unsafe()
	rows, err := db.Queryx(`SELECT * FROM pg_stat_database`)
	if err != nil {
		logger.Errorf("Failed to select pg_stat_database. %s", err)
		return nil, err
	}

	type pgStat struct {
		XactCommit   uint64   `db:"xact_commit"`
		XactRollback uint64   `db:"xact_rollback"`
		BlksRead     uint64   `db:"blks_read"`
		BlksHit      uint64   `db:"blks_hit"`
		BlkReadTime  *float64 `db:"blk_read_time"`
		BlkWriteTime *float64 `db:"blk_write_time"`
		TupReturned  uint64   `db:"tup_returned"`
		TupFetched   uint64   `db:"tup_fetched"`
		TupInserted  uint64   `db:"tup_inserted"`
		TupUpdated   uint64   `db:"tup_updated"`
		TupDeleted   uint64   `db:"tup_deleted"`
		Deadlocks    *uint64  `db:"deadlocks"`
		TempBytes    *uint64  `db:"temp_bytes"`
	}

	totalStat := pgStat{}
	for rows.Next() {
		p := pgStat{}
		if err := rows.StructScan(&p); err != nil {
			logger.Warningf("Failed to scan. %s", err)
			continue
		}
		totalStat.XactCommit += p.XactCommit
		totalStat.XactRollback += p.XactRollback
		totalStat.BlksRead += p.BlksRead
		totalStat.BlksHit += p.BlksHit
		if p.BlkReadTime != nil {
			if totalStat.BlkReadTime == nil {
				totalStat.BlkReadTime = p.BlkReadTime
			} else {
				*totalStat.BlkReadTime += *p.BlkReadTime
			}
		}
		if p.BlkWriteTime != nil {
			if totalStat.BlkWriteTime == nil {
				totalStat.BlkWriteTime = p.BlkWriteTime
			} else {
				*totalStat.BlkWriteTime += *p.BlkWriteTime
			}
		}
		totalStat.TupReturned += p.TupReturned
		totalStat.TupFetched += p.TupFetched
		totalStat.TupInserted += p.TupInserted
		totalStat.TupUpdated += p.TupUpdated
		totalStat.TupDeleted += p.TupDeleted
		if p.Deadlocks != nil {
			if totalStat.Deadlocks == nil {
				totalStat.Deadlocks = p.Deadlocks
			} else {
				*totalStat.Deadlocks += *p.Deadlocks
			}
		}
		if p.TempBytes != nil {
			if totalStat.TempBytes == nil {
				totalStat.TempBytes = p.TempBytes
			} else {
				*totalStat.TempBytes += *p.TempBytes
			}
		}
	}
	stat := make(map[string]interface{})
	stat["xact_commit"] = totalStat.XactCommit
	stat["xact_rollback"] = totalStat.XactRollback
	stat["blks_read"] = totalStat.BlksRead
	stat["blks_hit"] = totalStat.BlksHit
	if totalStat.BlkReadTime != nil {
		stat["blk_read_time"] = *totalStat.BlkReadTime
	}
	if totalStat.BlkWriteTime != nil {
		stat["blk_write_time"] = *totalStat.BlkWriteTime
	}
	stat["tup_returned"] = totalStat.TupReturned
	stat["tup_fetched"] = totalStat.TupFetched
	stat["tup_inserted"] = totalStat.TupInserted
	stat["tup_updated"] = totalStat.TupUpdated
	stat["tup_deleted"] = totalStat.TupDeleted
	if totalStat.Deadlocks != nil {
		stat["deadlocks"] = *totalStat.Deadlocks
	}
	if totalStat.TempBytes != nil {
		stat["temp_bytes"] = *totalStat.TempBytes
	}
	return stat, nil
}

func fetchConnections(db *sqlx.DB, version version) (map[string]interface{}, error) {
	var query string

	if version.first > 9 || version.first == 9 && version.second >= 6 {
		query = `select count(*), state, wait_event is not null from pg_stat_activity group by state, wait_event is not null`
	} else {
		query = `select count(*), state, waiting from pg_stat_activity group by state, waiting`
	}
	rows, err := db.Query(query)
	if err != nil {
		logger.Errorf("Failed to select pg_stat_activity. %s", err)
		return nil, err
	}

	stat := map[string]interface{}{
		"active":                      0.0,
		"active_waiting":              0.0,
		"idle":                        0.0,
		"idle_in_transaction":         0.0,
		"idle_in_transaction_aborted": 0.0,
	}

	normalizeRe := regexp.MustCompile("[^a-zA-Z0-9_-]+")

	for rows.Next() {
		var count float64
		var waiting bool
		var state string
		if err := rows.Scan(&count, &state, &waiting); err != nil {
			logger.Warningf("Failed to scan %s", err)
			continue
		}
		state = normalizeRe.ReplaceAllString(state, "_")
		state = strings.TrimRight(state, "_")
		if waiting {
			state += "_waiting"
		}
		stat[state] = float64(count)
	}

	return stat, nil
}

func fetchDatabaseSize(db *sqlx.DB) (map[string]interface{}, error) {
	rows, err := db.Query("select sum(pg_database_size(datname)) as dbsize from pg_database where has_database_privilege(datname, 'connect')")
	if err != nil {
		logger.Errorf("Failed to select pg_database_size. %s", err)
		return nil, err
	}

	var totalSize float64
	for rows.Next() {
		var dbsize float64
		if err := rows.Scan(&dbsize); err != nil {
			logger.Warningf("Failed to scan %s", err)
			continue
		}
		totalSize += dbsize
	}

	return map[string]interface{}{
		"total_size": totalSize,
	}, nil
}

var versionRe = regexp.MustCompile("PostgreSQL (\\d+)\\.(\\d+)(\\.(\\d+))? ")

type version struct {
	first  uint
	second uint
	thrird uint
}

func fetchVersion(db *sqlx.DB) (version, error) {

	res := version{}

	rows, err := db.Query("select version()")
	if err != nil {
		logger.Errorf("Failed to select version(). %s", err)
		return res, err
	}

	for rows.Next() {
		var versionStr string
		var first, second, third uint64
		if err := rows.Scan(&versionStr); err != nil {
			return res, err
		}

		// ref. https://www.postgresql.org/support/versioning/

		submatch := versionRe.FindStringSubmatch(versionStr)
		if len(submatch) >= 4 {
			first, err = strconv.ParseUint(submatch[1], 10, 0)
			if err != nil {
				return res, err
			}
			second, err = strconv.ParseUint(submatch[2], 10, 0)
			if err != nil {
				return res, err
			}
			if len(submatch) == 5 {
				third, err = strconv.ParseUint(submatch[4], 10, 0)
				if err != nil {
					return res, err
				}
			}
			res = version{uint(first), uint(second), uint(third)}
			return res, err
		}
	}
	return res, errors.New("failed to select version()")
}

func mergeStat(dst, src map[string]interface{}) {
	for k, v := range src {
		dst[k] = v
	}
}

// MetricKeyPrefix retruns the metrics key prefix
func (p PostgresPlugin) MetricKeyPrefix() string {
	if p.Prefix == "" {
		p.Prefix = "postgres"
	}
	return p.Prefix
}

// FetchMetrics interface for mackerelplugin
func (p PostgresPlugin) FetchMetrics() (map[string]interface{}, error) {

	db, err := sqlx.Connect("postgres", fmt.Sprintf("user=%s password=%s host=%s port=%s sslmode=%s connect_timeout=%d %s", p.Username, p.Password, p.Host, p.Port, p.SSLmode, p.Timeout, p.Option))
	if err != nil {
		logger.Errorf("FetchMetrics: %s", err)
		return nil, err
	}
	defer db.Close()

	version, err := fetchVersion(db)
	if err != nil {
		logger.Warningf("FetchMetrics: %s", err)
		return nil, err
	}

	statStatDatabase, err := fetchStatDatabase(db)
	if err != nil {
		return nil, err
	}
	statConnections, err := fetchConnections(db, version)
	if err != nil {
		return nil, err
	}
	statDatabaseSize, err := fetchDatabaseSize(db)
	if err != nil {
		return nil, err
	}

	stat := make(map[string]interface{})
	mergeStat(stat, statStatDatabase)
	mergeStat(stat, statConnections)
	mergeStat(stat, statDatabaseSize)

	return stat, err
}

// GraphDefinition interface for mackerelplugin
func (p PostgresPlugin) GraphDefinition() map[string]mp.Graphs {
	labelPrefix := strings.Title(p.MetricKeyPrefix())

	var graphdef = map[string]mp.Graphs{
		"connections": {
			Label: (labelPrefix + " Connections"),
			Unit:  "integer",
			Metrics: []mp.Metrics{
				{Name: "active", Label: "Active", Diff: false, Stacked: true},
				{Name: "active_waiting", Label: "Active waiting", Diff: false, Stacked: true},
				{Name: "idle", Label: "Idle", Diff: false, Stacked: true},
				{Name: "idle_in_transaction", Label: "Idle in transaction", Diff: false, Stacked: true},
				{Name: "idle_in_transaction_aborted_", Label: "Idle in transaction (aborted)", Diff: false, Stacked: true},
				{Name: "fastpath_function_call", Label: "fast-path function call", Diff: false, Stacked: true},
				{Name: "disabled", Label: "Disabled", Diff: false, Stacked: true},
			},
		},
		"commits": {
			Label: (labelPrefix + " Commits"),
			Unit:  "integer",
			Metrics: []mp.Metrics{
				{Name: "xact_commit", Label: "Xact Commit", Diff: true, Stacked: false},
				{Name: "xact_rollback", Label: "Xact Rollback", Diff: true, Stacked: false},
			},
		},
		"blocks": {
			Label: (labelPrefix + " Blocks"),
			Unit:  "integer",
			Metrics: []mp.Metrics{
				{Name: "blks_read", Label: "Blocks Read", Diff: true, Stacked: false},
				{Name: "blks_hit", Label: "Blocks Hit", Diff: true, Stacked: false},
			},
		},
		"rows": {
			Label: (labelPrefix + " Rows"),
			Unit:  "integer",
			Metrics: []mp.Metrics{
				{Name: "tup_returned", Label: "Returned Rows", Diff: true, Stacked: false},
				{Name: "tup_fetched", Label: "Fetched Rows", Diff: true, Stacked: true},
				{Name: "tup_inserted", Label: "Inserted Rows", Diff: true, Stacked: true},
				{Name: "tup_updated", Label: "Updated Rows", Diff: true, Stacked: true},
				{Name: "tup_deleted", Label: "Deleted Rows", Diff: true, Stacked: true},
			},
		},
		"size": {
			Label: (labelPrefix + " Data Size"),
			Unit:  "integer",
			Metrics: []mp.Metrics{
				{Name: "total_size", Label: "Total Size", Diff: false, Stacked: false},
			},
		},
		"deadlocks": {
			Label: (labelPrefix + " Dead Locks"),
			Unit:  "integer",
			Metrics: []mp.Metrics{
				{Name: "deadlocks", Label: "Deadlocks", Diff: true, Stacked: false},
			},
		},
		"iotime": {
			Label: (labelPrefix + " Block I/O time"),
			Unit:  "float",
			Metrics: []mp.Metrics{
				{Name: "blk_read_time", Label: "Block Read Time (ms)", Diff: true, Stacked: false},
				{Name: "blk_write_time", Label: "Block Write Time (ms)", Diff: true, Stacked: false},
			},
		},
		"tempfile": {
			Label: (labelPrefix + " Temporary file"),
			Unit:  "integer",
			Metrics: []mp.Metrics{
				{Name: "temp_bytes", Label: "Temporary file size (byte)", Diff: true, Stacked: false},
			},
		},
	}

	return graphdef
}

// Do the plugin
func Do() {
	optHost := flag.String("hostname", "localhost", "Hostname to login to")
	optPort := flag.String("port", "5432", "Database port")
	optUser := flag.String("user", "", "Postgres User")
	optDatabase := flag.String("database", "", "Database name")
	optPass := flag.String("password", "", "Postgres Password")
	optPrefix := flag.String("metric-key-prefix", "postgres", "Metric key prefix")
	optSSLmode := flag.String("sslmode", "disable", "Whether or not to use SSL")
	optConnectTimeout := flag.Int("connect_timeout", 5, "Maximum wait for connection, in seconds.")
	optTempfile := flag.String("tempfile", "", "Temp file name")
	flag.Parse()

	if *optUser == "" {
		logger.Warningf("user is required")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if *optPass == "" {
		logger.Warningf("password is required")
		flag.PrintDefaults()
		os.Exit(1)
	}
	option := ""
	if *optDatabase != "" {
		option = fmt.Sprintf("dbname=%s", *optDatabase)
	}

	var postgres PostgresPlugin
	postgres.Host = *optHost
	postgres.Port = *optPort
	postgres.Username = *optUser
	postgres.Password = *optPass
	postgres.Prefix = *optPrefix
	postgres.SSLmode = *optSSLmode
	postgres.Timeout = *optConnectTimeout
	postgres.Option = option

	helper := mp.NewMackerelPlugin(postgres)

	helper.Tempfile = *optTempfile
	helper.Run()
}
