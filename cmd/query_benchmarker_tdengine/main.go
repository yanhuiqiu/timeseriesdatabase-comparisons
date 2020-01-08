// query_benchmarker speed tests InfluxDB using requests from stdin.
//
// It reads encoded Query objects from stdin, and makes concurrent requests
// to the provided HTTP endpoint. This program has no knowledge of the
// internals of the endpoint.
package main

import (
	"encoding/gob"
	"encoding/base64"
	"database/sql"
	"flag"
	"fmt"
	"github.com/liu0x54/timeseriesdatabase-comparisons/bulk_query"
	"github.com/liu0x54/timeseriesdatabase-comparisons/bulk_query/http"
	"github.com/liu0x54/timeseriesdatabase-comparisons/util/report"
	_ "github.com/taosdata/TDengine/src/connector/go/src/taosSql"	
	"io"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Program option vars:
type TDengineQueryBenchmarker struct {
	csvDaemonUrls string
	daemonUrls    []string

	dialTimeout        time.Duration
	readTimeout        time.Duration
	writeTimeout       time.Duration
	httpClientType     string
	clientIndex        int
	scanFinished       bool
	queryPool sync.Pool
	queryChan chan []*http.Query
}
var cgo int = 0
var querier = &TDengineQueryBenchmarker{}
var taosDriverName string = "taosSql"
// Parse args:
func init() {

	bulk_query.Benchmarker.Init()
	querier.Init()

	flag.Parse()

	bulk_query.Benchmarker.Validate()
	querier.Validate()

}

func (b *TDengineQueryBenchmarker) Init() {
	flag.StringVar(&b.csvDaemonUrls, "urls", "http://localhost:6020", "Daemon URLs, comma-separated. Will be used in a round-robin fashion.")
	flag.DurationVar(&b.dialTimeout, "dial-timeout", time.Second*15, "TCP dial timeout.")
	flag.DurationVar(&b.readTimeout, "write-timeout", time.Second*300, "TCP write timeout.")
	flag.DurationVar(&b.writeTimeout, "read-timeout", time.Second*300, "TCP read timeout.")
	flag.StringVar(&b.httpClientType, "http-client-type", "fast", "HTTP client type {fast, default,cgo}")
	flag.IntVar(&b.clientIndex, "client-index", 0, "Index of a client host running this tool. Used to distribute load")
}

func (b *TDengineQueryBenchmarker) Validate() {
	b.daemonUrls = strings.Split(b.csvDaemonUrls, ",")
	if len(b.daemonUrls) == 0 {
		log.Fatal("missing 'urls' flag")
	}
	fmt.Printf("daemon URLs: %v\n", b.daemonUrls)

	if b.httpClientType == "fast" || b.httpClientType == "default" {
		fmt.Printf("Using HTTP client: %v\n", b.httpClientType)
		http.UseFastHttp = b.httpClientType == "fast"
	} else if b.httpClientType == "cgo" {
		fmt.Printf("Using TDengine C connector: %v\n", b.httpClientType)	
		cgo = 1	
	} else {
		log.Fatalf("Unsupported HTPP client type: %v", b.httpClientType)
	}
}

func (b *TDengineQueryBenchmarker) Prepare() {
	// Make pools to minimize heap usage:
	b.queryPool = sync.Pool{
		New: func() interface{} {
			return &http.Query{
				HumanLabel:       make([]byte, 0, 1024),
				HumanDescription: make([]byte, 0, 1024),
				Method:           make([]byte, 0, 1024),
				Path:             make([]byte, 0, 1024),
				Body:             make([]byte, 0, 1024),
			}
		},
	}

	// Make data and control channels:
	b.queryChan = make(chan []*http.Query)
}

func (b *TDengineQueryBenchmarker) GetProcessor() bulk_query.Processor {
	return b
}
func (b *TDengineQueryBenchmarker) GetScanner() bulk_query.Scanner {
	return b
}

func (b *TDengineQueryBenchmarker) PrepareProcess(i int) {
}

func (b *TDengineQueryBenchmarker) RunProcess(i int, workersGroup *sync.WaitGroup, statPool sync.Pool, statChan chan *bulk_query.Stat) {
	daemonUrl := b.daemonUrls[(i+b.clientIndex)%len(b.daemonUrls)]
	w := http.NewHTTPClient(daemonUrl, bulk_query.Benchmarker.Debug(), b.dialTimeout, b.readTimeout, b.writeTimeout)
	b.processQueries(w, workersGroup, statPool, statChan)
}

func (b *TDengineQueryBenchmarker) IsScanFinished() bool {
	return b.scanFinished
}

func (b *TDengineQueryBenchmarker) CleanUp() {
	close(b.queryChan)
}

func (b TDengineQueryBenchmarker) UpdateReport(params *report.QueryReportParams, reportTags [][2]string, extraVals []report.ExtraVal) (updatedTags [][2]string, updatedExtraVals []report.ExtraVal) {
	params.DBType = "TDengine"
	params.DestinationUrl = b.csvDaemonUrls
	updatedTags = reportTags
	updatedExtraVals = extraVals
	return
}

func main() {
	bulk_query.Benchmarker.RunBenchmark(querier)
}

var qind int64

// scan reads encoded Queries and places them onto the workqueue.
func (b *TDengineQueryBenchmarker) RunScan(r io.Reader, closeChan chan int) {
	dec := gob.NewDecoder(r)

	batch := make([]*http.Query, 0, bulk_query.Benchmarker.BatchSize())

	i := 0
loop:
	for {
		if bulk_query.Benchmarker.Limit() >= 0 && qind >= bulk_query.Benchmarker.Limit() {
			break
		}

		q := b.queryPool.Get().(*http.Query)
		err := dec.Decode(q)
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}

		q.ID = qind
		batch = append(batch, q)
		i++
		if i == bulk_query.Benchmarker.BatchSize() {
			b.queryChan <- batch
			//batch = batch[:0]
			batch = nil
			batch = make([]*http.Query, 0, bulk_query.Benchmarker.BatchSize())
			i = 0
		}

		qind++
		select {
		case <-closeChan:
			log.Println("Received finish request")
			break loop
		default:
		}

	}
	b.scanFinished = true
}

// processQueries reads byte buffers from queryChan and writes them to the
// target server, while tracking latency.
func (b *TDengineQueryBenchmarker) processQueries(w http.HTTPClient, workersGroup *sync.WaitGroup, statPool sync.Pool, statChan chan *bulk_query.Stat) error {
	restAuthorization := fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte("root:taosdata")))
	opts := &http.HTTPClientDoOptions{
		Authorization:        restAuthorization,
		Debug:                bulk_query.Benchmarker.Debug(),
		PrettyPrintResponses: bulk_query.Benchmarker.PrettyPrintResponses(),
	}
	var queriesSeen int64
	for queries := range b.queryChan {
		if len(queries) == 1 {
			if err := b.processSingleQuery(w, queries[0], opts, nil, nil, statPool, statChan); err != nil {
				log.Fatal(err)
			}
			queriesSeen++
		} else {
			var err error
			errors := 0
			done := 0
			errCh := make(chan error)
			doneCh := make(chan int, len(queries))
			for _, q := range queries {
				go b.processSingleQuery(w, q, opts, errCh, doneCh, statPool, statChan)
				queriesSeen++
				if bulk_query.Benchmarker.GradualWorkersIncrease() {
					time.Sleep(time.Duration(rand.Int63n(150)) * time.Millisecond) // random sleep 0-150ms
				}
			}

		loop:
			for {
				select {
				case err = <-errCh:
					errors++
				case <-doneCh:
					done++
					if done == len(queries) {
						break loop
					}
				}
			}
			close(errCh)
			close(doneCh)
			if err != nil {
				log.Fatal(err)
			}
		}
		if bulk_query.Benchmarker.WaitInterval().Seconds() > 0 {
			time.Sleep(bulk_query.Benchmarker.WaitInterval())
		}
	}
	workersGroup.Done()
	return nil
}

func (b *TDengineQueryBenchmarker) processSingleQuery(w http.HTTPClient, q *http.Query, opts *http.HTTPClientDoOptions, errCh chan error, doneCh chan int, statPool sync.Pool, statChan chan *bulk_query.Stat) error {
	defer func() {
		if doneCh != nil {
			doneCh <- 1
		}
	}()
	var lagMillis float64
	var err error
	if cgo ==1 {
		lagMillis, err = b.execSql(q)
	}else {
		lagMillis, err = w.Do(q, opts)
	}
	stat := statPool.Get().(*bulk_query.Stat)
	stat.Init(q.HumanLabel, lagMillis)
	statChan <- stat
	b.queryPool.Put(q)
	if err != nil {
		qerr := fmt.Errorf("Error during request of query %s: %s\n", q.String(), err.Error())
		if errCh != nil {
			errCh <- qerr
			return nil
		} else {
			return qerr
		}
	}

	return nil
}

func (b *TDengineQueryBenchmarker)execSql(q *http.Query) (lag float64, err error) {
	db, err := sql.Open(taosDriverName, "root:taosdata@/tcp("+b.csvDaemonUrls+")/")
	if err != nil {
		log.Fatalf("Open database error: %s\n", err)
	}
	defer db.Close()
	sqlcmd:=string(q.Body)
	start := time.Now()
	_, err = db.Exec(sqlcmd)
	lag = float64(time.Since(start).Nanoseconds()) / 1e6 // milliseconds
	if err != nil {
		log.Fatalf("Query error: %s\n", err)
	}
	return lag, err 
}