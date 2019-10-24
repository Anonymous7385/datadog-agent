package testutil

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
)

// DummyECS allows tests to mock a ECS's responses
type DummyECS struct {
	mux          *http.ServeMux
	fileHandlers map[string]string
	rawHandlers  map[string]string
	Requests     chan *http.Request
}

type option func(*DummyECS)

func FileHandlerOption(pattern, testDataFile string) option {
	return func(d *DummyECS) {
		d.fileHandlers[pattern] = testDataFile
	}
}

func RawHandlerOption(pattern, rawResponse string) option {
	return func(d *DummyECS) {
		d.rawHandlers[pattern] = rawResponse
	}
}

// NewDummyECS create a mock of the ECS api
func NewDummyECS(ops ...option) (*DummyECS, error) {
	d := &DummyECS{
		mux:          http.NewServeMux(),
		fileHandlers: make(map[string]string),
		rawHandlers:  make(map[string]string),
		Requests:     make(chan *http.Request, 3),
	}
	for _, o := range ops {
		o(d)
	}
	for pattern, testDataPath := range d.fileHandlers {
		raw, err := ioutil.ReadFile(testDataPath)
		if err != nil {
			return nil, fmt.Errorf("failed to register handler for pattern %s: could not read test data file with path %s", pattern, testDataPath)
		}
		d.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Write(raw)
		})
	}
	for pattern, rawData := range d.rawHandlers {
		d.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(rawData))
		})
	}
	return d, nil
}

func (d *DummyECS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("dummyECS received %s on %s", r.Method, r.URL.Path)
	d.Requests <- r
	d.mux.ServeHTTP(w, r)
}

// Start starts the HTTP server
func (d *DummyECS) Start() (*httptest.Server, int, error) {
	ts := httptest.NewServer(d)
	ecsAgentURL, err := url.Parse(ts.URL)
	if err != nil {
		return nil, 0, err
	}
	ecsAgentPort, err := strconv.Atoi(ecsAgentURL.Port())
	if err != nil {
		return nil, 0, err
	}
	return ts, ecsAgentPort, nil
}
