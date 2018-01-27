package main

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/miku/esbulk"
)

// Application Version.
const Version = "0.4.9"

func main() {

	version := flag.Bool("v", false, "prints current program version")
	cpuprofile := flag.String("cpuprofile", "", "write cpu profile to file")
	memprofile := flag.String("memprofile", "", "write heap profile to file")
	indexName := flag.String("index", "", "index name")
	docType := flag.String("type", "default", "elasticsearch doc type")
	server := flag.String("server", "http://localhost:9200", "elasticsearch server, this works with https as well")
	host := flag.String("host", "localhost", "elasticsearch host (deprecated: use -server instead)")
	port := flag.Int("port", 9200, "elasticsearch port (deprecated: use -server instead)")
	batchSize := flag.Int("size", 1000, "bulk batch size")
	numWorkers := flag.Int("w", runtime.NumCPU(), "number of workers to use")
	verbose := flag.Bool("verbose", false, "output basic progress")
	gzipped := flag.Bool("z", false, "unzip gz'd file on the fly")
	mapping := flag.String("mapping", "", "mapping string or filename to apply before indexing")
	purge := flag.Bool("purge", false, "purge any existing index before indexing")
	idfield := flag.String("id", "", "name of field to use as id field, by default ids are autogenerated")
	user := flag.String("u", "", "http basic auth username:password, like curl -u")

	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if *version {
		fmt.Println(Version)
		os.Exit(0)
	}

	if *indexName == "" {
		log.Fatal("index name required")
	}

	var file io.Reader = os.Stdin

	if flag.NArg() > 0 {
		f, err := os.Open(flag.Arg(0))
		if err != nil {
			log.Fatalln(err)
		}
		defer f.Close()
		file = f
	}

	runtime.GOMAXPROCS(*numWorkers)

	var username, password string
	if len(*user) > 0 {
		parts := strings.Split(*user, ":")
		if len(parts) != 2 {
			log.Fatal("http basic auth syntax is: username:password")
		}
		username = parts[0]
		password = parts[1]
	}

	options := esbulk.Options{
		Host:      *host,
		Port:      *port,
		Index:     *indexName,
		DocType:   *docType,
		BatchSize: *batchSize,
		Verbose:   *verbose,
		Scheme:    "http",
		IDField:   *idfield,
		Username:  username,
		Password:  password,
	}

	// backwards-compat for -host and -port, only use newer -server flag if
	// older -host and -port are on defaults
	if *host == "localhost" && *port == 9200 {
		if err := options.SetServer(*server); err != nil {
			log.Fatal(err)
		}
	}

	if *purge {
		if err := esbulk.DeleteIndex(options); err != nil {
			log.Fatal(err)
		}
	}

	// create index if not exists
	if err := esbulk.CreateIndex(options); err != nil {
		log.Fatal(err)
	}

	if *mapping != "" {
		var reader io.Reader
		if _, err := os.Stat(*mapping); os.IsNotExist(err) {
			reader = strings.NewReader(*mapping)
		} else {
			file, err := os.Open(*mapping)
			if err != nil {
				log.Fatal(err)
			}
			reader = bufio.NewReader(file)
		}
		err := esbulk.PutMapping(options, reader)
		if err != nil {
			log.Fatal(err)
		}
	}

	queue := make(chan string)
	var wg sync.WaitGroup

	for i := 0; i < *numWorkers; i++ {
		wg.Add(1)
		go esbulk.Worker(fmt.Sprintf("worker-%d", i), options, queue, &wg)
	}

	client := &http.Client{}

	// shutdown procedure
	// TODO(miku): maybe handle signals, too
	defer func() {
		// reset refresh interval
		r := strings.NewReader(`{"index": {"refresh_interval": "1s"}}`)
		req, err := http.NewRequest("PUT", fmt.Sprintf("%s://%s:%d/%s/_settings",
			options.Scheme, options.Host, options.Port, options.Index), r)
		if err != nil {
			log.Fatal(err)
		}
		if options.Username != "" && options.Password != "" {
			req.SetBasicAuth(options.Username, options.Password)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		if options.Verbose {
			log.Printf("set index.refresh_interval to 1s: %s\n", resp.Status)
		}
		// flush
		link := fmt.Sprintf("%s://%s:%d/%s/_flush", options.Scheme, options.Host, options.Port, options.Index)
		req, err = http.NewRequest("POST", link, nil)
		if err != nil {
			log.Fatal(err)
		}
		if options.Username != "" && options.Password != "" {
			req.SetBasicAuth(options.Username, options.Password)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err = client.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		if options.Verbose {
			log.Printf("index flushed: %s\n", resp.Status)
		}
	}()

	r := strings.NewReader(`{"index": {"refresh_interval": "-1"}}`)
	req, err := http.NewRequest("PUT", fmt.Sprintf("%s://%s:%d/%s/_settings",
		options.Scheme, options.Host, options.Port, options.Index), r)
	if err != nil {
		log.Fatal(err)
	}
	if options.Username != "" && options.Password != "" {
		req.SetBasicAuth(options.Username, options.Password)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Fatal(resp)
	}
	if options.Verbose {
		log.Printf("set index.refresh_interval to -1: %s\n", resp.Status)
	}

	reader := bufio.NewReader(file)
	if *gzipped {
		zreader, err := gzip.NewReader(file)
		if err != nil {
			log.Fatal(err)
		}
		reader = bufio.NewReader(zreader)
	}

	counter := 0
	start := time.Now()

	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		queue <- line
		counter++
	}

	close(queue)
	wg.Wait()
	elapsed := time.Since(start)

	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.WriteHeapProfile(f)
		f.Close()
	}

	if *verbose {
		rate := float64(counter) / elapsed.Seconds()
		log.Printf("%d docs in %s at %0.3f docs/s with %d workers\n", counter, elapsed, rate, *numWorkers)
	}
}
