package esbulk

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/sethgrid/pester"
)

// Version of application.
var Version = "dev" // next: 0.6.3

// Runner bundles various options. Factored out of a former main func and
// should be further split up (TODO).
type Runner struct {
	BatchSize       int
	CpuProfile      string
	DocType         string
	File            *os.File
	FileGzipped     bool
	IdentifierField string
	IndexName       string
	Mapping         string
	MemProfile      string
	NumWorkers      int
	Password        string
	Pipeline        string
	Purge           bool
	RefreshInterval string
	Scheme          string
	Servers         []string
	ShowVersion     bool
	SkipBroken      bool
	Username        string
	Verbose         bool
	ZeroReplica     bool
}

// Run starts indexing documents from file into a given index.
func (r *Runner) Run() (err error) {
	if r.ShowVersion {
		fmt.Println(Version)
		return nil
	}
	if r.CpuProfile != "" {
		f, err := os.Create(r.CpuProfile)
		if err != nil {
			return err
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	if r.IndexName == "" {
		return fmt.Errorf("index name required")
	}
	if len(r.Servers) == 0 {
		r.Servers = append(r.Servers, "http://localhost:9200")
	}
	if r.Verbose {
		log.Printf("using %d server(s)", len(r.Servers))
	}
	options := Options{
		Servers:   r.Servers,
		Index:     r.IndexName,
		DocType:   r.DocType,
		BatchSize: r.BatchSize,
		Verbose:   r.Verbose,
		Scheme:    "http",
		IDField:   r.IdentifierField,
		Username:  r.Username,
		Password:  r.Password,
		Pipeline:  r.Pipeline,
	}
	if r.Verbose {
		log.Println(options)
	}
	if r.Purge {
		if err := DeleteIndex(options); err != nil {
			return err
		}
		time.Sleep(5 * time.Second)
	}
	if err := CreateIndex(options); err != nil {
		return err
	}
	if r.Mapping != "" {
		var reader io.Reader
		if _, err := os.Stat(r.Mapping); os.IsNotExist(err) {
			reader = strings.NewReader(r.Mapping)
		} else {
			file, err := os.Open(r.Mapping)
			if err != nil {
				return err
			}
			reader = bufio.NewReader(file)
		}
		err := PutMapping(options, reader)
		if err != nil {
			return err
		}
	}
	var (
		queue = make(chan string)
		wg    sync.WaitGroup
	)
	wg.Add(r.NumWorkers)
	for i := 0; i < r.NumWorkers; i++ {
		name := fmt.Sprintf("worker-%d", i)
		go Worker(name, options, queue, &wg)
	}
	for i, _ := range options.Servers {
		// Store number_of_replicas settings for restoration later.
		doc, err := GetSettings(i, options)
		if err != nil {
			return err
		}
		// TODO(miku): Rework this.
		numberOfReplicas := doc[options.Index].(map[string]interface{})["settings"].(map[string]interface{})["index"].(map[string]interface{})["number_of_replicas"]
		if r.Verbose {
			log.Printf("on shutdown, number_of_replicas will be set back to %s", numberOfReplicas)
		}
		if r.Verbose {
			log.Printf("on shutdown, refresh_interval will be set back to %s", r.RefreshInterval)
		}
		// Shutdown procedure. TODO(miku): Handle signals, too.
		defer func() {
			// Realtime search.
			if _, err = indexSettingsRequest(fmt.Sprintf(`{"index": {"refresh_interval": "%s"}}`, r.RefreshInterval), options); err != nil {
				return
			}
			// Reset number of replicas.
			if _, err = indexSettingsRequest(fmt.Sprintf(`{"index": {"number_of_replicas": %q}}`, numberOfReplicas), options); err != nil {
				return
			}
			// Persist documents.
			err = FlushIndex(i, options)
		}()
		// Realtime search.
		resp, err := indexSettingsRequest(`{"index": {"refresh_interval": "-1"}}`, options)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			b, err := httputil.DumpResponse(resp, true)
			if err != nil {
				return err
			}
			return fmt.Errorf("got %v: %v", resp.StatusCode, string(b))
		}
		if r.ZeroReplica {
			// Reset number of replicas.
			if _, err := indexSettingsRequest(`{"index": {"number_of_replicas": 0}}`, options); err != nil {
				return err
			}
		}
	}
	var (
		reader  = bufio.NewReader(r.File)
		counter = 0
		start   = time.Now()
	)
	if r.FileGzipped {
		zreader, err := gzip.NewReader(r.File)
		if err != nil {
			log.Fatal(err)
		}
		reader = bufio.NewReader(zreader)
	}
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if line = strings.TrimSpace(line); len(line) == 0 {
			continue
		}
		if r.SkipBroken {
			if !(IsJSON(line)) {
				if r.Verbose {
					fmt.Printf("skipped line [%s]\n", line)
				}
				continue
			}
		}
		queue <- line
		counter++
	}
	close(queue)
	wg.Wait()
	elapsed := time.Since(start)
	if r.MemProfile != "" {
		f, err := os.Create(r.MemProfile)
		if err != nil {
			return err
		}
		pprof.WriteHeapProfile(f)
		f.Close()
	}
	if r.Verbose {
		rate := float64(counter) / elapsed.Seconds()
		log.Printf("%d docs in %s at %0.3f docs/s with %d workers\n", counter, elapsed, rate, r.NumWorkers)
	}
	return nil
}

// indexSettingsRequest runs updates an index setting, given a body and
// options. Body consist of the JSON document, e.g. `{"index":
// {"refresh_interval": "1s"}}`.
func indexSettingsRequest(body string, options Options) (*http.Response, error) {
	r := strings.NewReader(body)

	rand.Seed(time.Now().Unix())
	server := options.Servers[rand.Intn(len(options.Servers))]
	link := fmt.Sprintf("%s/%s/_settings", server, options.Index)

	req, err := http.NewRequest("PUT", link, r)
	if err != nil {
		return nil, err
	}
	// Auth handling.
	if options.Username != "" && options.Password != "" {
		req.SetBasicAuth(options.Username, options.Password)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := pester.Do(req)
	if err != nil {
		return nil, err
	}
	if options.Verbose {
		log.Printf("applied setting: %s with status %s\n", body, resp.Status)
	}
	return resp, nil
}

// IsJSON checks if a string is valid json.
func IsJSON(str string) bool {
	var js json.RawMessage
	return json.Unmarshal([]byte(str), &js) == nil
}
